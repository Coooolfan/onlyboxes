use super::*;
use sha2::{Digest, Sha256};

fn large_source() -> Operation {
    let mut op = source();
    op.restore_spec
        .as_mut()
        .unwrap()
        .env
        .insert("SHARED_PRIVATE_CONFIGURATION".into(), "x".repeat(16 * 1024));
    op
}

#[test]
fn codec_shares_specs_and_reopens_every_logical_record_without_changing_authority() {
    let root = Root::new();
    let j = root.open();
    let base = large_source();
    j.claim(base.clone()).unwrap();
    freeze(&j);
    commit(&j);
    let mut restore = target(2);
    restore.restore_spec = base.restore_spec.clone();
    j.claim(restore).unwrap();
    j.transaction(|state| {
        for index in 0..120 {
            let mut op = base.clone();
            op.operation_id = format!("pending-{index}");
            op.scoped_session_id = format!("obx:owner:pending-{index}");
            op.box_id = format!("pending_box_{index}");
            state
                .current
                .insert(op.scoped_session_id.clone(), op.operation_id.clone());
            state.operations.insert(op.operation_id.clone(), op);
        }
        let mut active = base.clone();
        active.operation_id = "live-adoption".into();
        active.scoped_session_id = "obx:owner:live".into();
        active.kind = Kind::Adopt;
        active.phase = Phase::Active;
        active.staging_bytes = 0;
        state.current.insert(
            active.scoped_session_id.clone(),
            active.operation_id.clone(),
        );
        state.operations.insert(active.operation_id.clone(), active);
        let mut deleted = deletion();
        deleted.scoped_session_id = "obx:owner:deleted".into();
        deleted.restore_spec = base.restore_spec.clone();
        deleted.phase = Phase::Deleted;
        deleted.tombstone_box_id = "deleted_marker".into();
        state.current.insert(
            deleted.scoped_session_id.clone(),
            deleted.operation_id.clone(),
        );
        state
            .operations
            .insert(deleted.operation_id.clone(), deleted);
        let mut different = base.clone();
        different.operation_id = "different-config".into();
        different.scoped_session_id = "obx:another:session".into();
        different.restore_spec.as_mut().unwrap().memory_mib += 1;
        state.current.insert(
            different.scoped_session_id.clone(),
            different.operation_id.clone(),
        );
        state
            .operations
            .insert(different.operation_id.clone(), different);
        Ok(())
    })
    .unwrap();
    let before = serde_json::to_vec(&j.snapshot().unwrap()).unwrap();
    let disk = fs::read(root.journal().join("journal.json")).unwrap();
    let encoded: serde_json::Value = serde_json::from_slice(&disk).unwrap();
    assert_eq!(encoded["version"], 2);
    assert_eq!(encoded["restore_specs"].as_object().unwrap().len(), 2);
    for op in encoded["operations"].as_object().unwrap().values() {
        assert!(op.get("restore_spec").is_none());
        assert_eq!(op["restore_spec_sha256"].as_str().unwrap().len(), 64);
    }
    assert!(disk.len() * 5 < before.len());
    println!(
        "spec-sharing journal bytes: full={} compact={}",
        before.len(),
        disk.len()
    );
    j.close().unwrap();
    let reopened = root.open();
    let state = reopened.snapshot().unwrap();
    assert!(
        before == serde_json::to_vec(&state).unwrap(),
        "logical journal changed after compaction/reopen"
    );
    assert!(state
        .operations
        .values()
        .all(|op| op.restore_spec_sha256.is_none()));
    let mut wrong = target(2);
    wrong.restore_spec = base.restore_spec;
    wrong.restore_spec.as_mut().unwrap().memory_mib += 1;
    assert_eq!(reopened.claim(wrong), Err(Error::Conflict));
    assert_eq!(
        reopened.cancel_unknown("late", "obx:owner:deleted", 999),
        Err(Error::Conflict)
    );
}

#[test]
fn codec_reads_v1_and_upgrades_only_on_a_successful_durable_mutation() {
    for lost_ack in [false, true] {
        let root = Root::new();
        let j = root.open();
        j.claim(large_source()).unwrap();
        let logical = j.snapshot().unwrap();
        j.close().unwrap();
        let path = root.journal().join("journal.json");
        let v1 = serde_json::to_vec(&logical).unwrap();
        fs::write(&path, &v1).unwrap();
        let mut reopened = root.open();
        assert!(
            fs::read(&path).unwrap() == v1,
            "read-only reopen rewrote v1"
        );
        reopened.persist = if lost_ack {
            |root, state| {
                persist_state(root, state)?;
                Err(Error::Unavailable)
            }
        } else {
            |_, _| Err(Error::Unavailable)
        };
        assert_eq!(
            reopened.update("suspend-1", |op| {
                op.phase = Phase::RollingBack;
                Ok(())
            }),
            Err(Error::Unavailable)
        );
        assert!(matches!(reopened.snapshot(), Err(Error::Unavailable)));
        reopened.close().unwrap();
        let recovered = root.open().snapshot().unwrap();
        assert_eq!(
            recovered.operations["suspend-1"].phase,
            if lost_ack {
                Phase::RollingBack
            } else {
                Phase::Preparing
            }
        );
        assert!(
            recovered.operations["suspend-1"].restore_spec
                == logical.operations["suspend-1"].restore_spec
        );
        let disk: serde_json::Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(disk["version"], if lost_ack { 2 } else { 1 });
    }
}

#[test]
fn codec_rejects_missing_tampered_inline_or_unreferenced_specs_without_rewriting() {
    let root = Root::new();
    let j = root.open();
    j.claim(large_source()).unwrap();
    j.close().unwrap();
    let path = root.journal().join("journal.json");
    let original: serde_json::Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    let digest = original["operations"]["suspend-1"]["restore_spec_sha256"]
        .as_str()
        .unwrap();
    let spec = original["restore_specs"][digest].clone();
    for case in [
        "missing",
        "tampered",
        "inline",
        "v1-reference",
        "unused",
        "unused-valid",
        "dangling",
        "cancel-reference",
    ] {
        let mut value = original.clone();
        match case {
            "missing" => {
                value["restore_specs"].as_object_mut().unwrap().clear();
            }
            "tampered" => {
                value["restore_specs"][digest]["memory_mib"] = 4096.into();
            }
            "inline" => {
                value["operations"]["suspend-1"]["restore_spec"] = spec.clone();
            }
            "v1-reference" => {
                value["version"] = 1.into();
            }
            "unused" => {
                value["operations"]["suspend-1"]
                    .as_object_mut()
                    .unwrap()
                    .remove("restore_spec_sha256");
            }
            "unused-valid" => {
                let mut extra: RestoreSpec = serde_json::from_value(spec.clone()).unwrap();
                extra.memory_mib += 1;
                let hash = format!("{:x}", Sha256::digest(serde_json::to_vec(&extra).unwrap()));
                value["restore_specs"][hash] = serde_json::to_value(extra).unwrap();
            }
            "dangling" => {
                value["operations"]["suspend-1"]["restore_spec_sha256"] = "f".repeat(64).into();
            }
            "cancel-reference" => {
                value["operations"]["cancel"] = serde_json::json!({"operation_id":"cancel", "scoped_session_id":"obx:owner:cancel", "generation":1, "kind":"cancel", "phase":"canceled", "restore_spec_sha256":digest});
                value["current"]["obx:owner:cancel"] = "cancel".into();
            }
            _ => unreachable!(),
        }
        let invalid = serde_json::to_vec(&value).unwrap();
        fs::write(&path, &invalid).unwrap();
        assert!(
            matches!(
                Journal::open(root.journal(), "worker-a"),
                Err(Error::Unavailable)
            ),
            "accepted {case}"
        );
        assert!(
            fs::read(&path).unwrap() == invalid,
            "rewrote rejected {case}"
        );
    }
}

#[test]
fn codec_references_cannot_be_submitted_as_runtime_operation_authority() {
    let root = Root::new();
    let j = root.open();
    let mut op = source();
    op.restore_spec_sha256 = Some("a".repeat(64));
    assert_eq!(j.claim(op), Err(Error::Conflict));
    j.claim(source()).unwrap();
    assert_eq!(
        j.update("suspend-1", |op| {
            op.restore_spec_sha256 = Some("a".repeat(64));
            Ok(())
        }),
        Err(Error::Conflict)
    );
    assert!(j.snapshot().unwrap().operations["suspend-1"]
        .restore_spec_sha256
        .is_none());
}

#[test]
fn codec_bounds_expansion_and_closes_admission_when_a_compact_write_exceeds_it() {
    let root = Root::new();
    let j = root.open();
    let base = large_source();
    j.claim(base.clone()).unwrap();
    let path = root.journal().join("journal.json");
    let before = fs::read(&path).unwrap();
    let mut compact: serde_json::Value = serde_json::from_slice(&before).unwrap();
    let stored = compact["operations"]["suspend-1"].clone();
    let fill = |state: &mut JournalState| {
        for index in 0..5000 {
            let mut op = base.clone();
            op.operation_id = format!("expansion-{index}");
            state.operations.insert(op.operation_id.clone(), op);
        }
    };
    assert_eq!(
        j.transaction(|state| {
            fill(state);
            Ok(())
        }),
        Err(Error::Unavailable)
    );
    assert!(matches!(j.snapshot(), Err(Error::Unavailable)));
    j.close().unwrap();
    assert!(fs::read(&path).unwrap() == before);
    for index in 0..5000 {
        let id = format!("expansion-{index}");
        let mut op = stored.clone();
        op["operation_id"] = id.clone().into();
        compact["operations"][id] = op;
    }
    let expanded = serde_json::to_vec(&compact).unwrap();
    assert!(expanded.len() < MAX_JOURNAL_BYTES / 4);
    fs::write(&path, &expanded).unwrap();
    assert!(matches!(
        Journal::open(root.journal(), "worker-a"),
        Err(Error::Unavailable)
    ));
    assert!(fs::read(&path).unwrap() == expanded);
}
