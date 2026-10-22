use super::*;
use crate::persistence::{Archive, RestoreSpec, ARCHIVE_FORMAT};
use std::io::{BufRead, BufReader};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::symlink;
use std::process::{Command, Stdio};
use std::sync::{Arc, Barrier};

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        let path = std::env::temp_dir().join(format!(
            "onlyboxes-boxlite-journal-{}",
            uuid::Uuid::new_v4()
        ));
        private_directory(&path).unwrap();
        Self(path)
    }
    fn journal(&self) -> PathBuf {
        self.0.join("journal")
    }
    fn open(&self) -> Journal {
        Journal::open(self.journal(), "worker-a").unwrap()
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

fn source() -> Operation {
    Operation {
        operation_id: "suspend-1".into(),
        scoped_session_id: "obx:owner:session".into(),
        generation: 1,
        kind: Kind::Source,
        phase: Phase::Preparing,
        restore_spec: Some(RestoreSpec {
            runtime: "boxlite".into(),
            runtime_version: "0.10.7".into(),
            platform: "linux/arm64".into(),
            format: ARCHIVE_FORMAT.into(),
            image: format!("image@sha256:{}", "a".repeat(64)),
            memory_mib: 512,
            cpus: 1.0,
            max_processes: 128,
            user: "root".into(),
            working_dir: "/workspace".into(),
            env: BTreeMap::from([("PRIVATE".into(), "private-value".into())]),
            entrypoint: vec!["sh".into(), "-lc".into()],
            command: vec!["sleep infinity".into()],
        }),
        archive: None,
        artifact: String::new(),
        box_id: "source_box_123".into(),
        tombstone_box_id: String::new(),
        name_fences: Default::default(),
        cleanup_processes: Default::default(),
        cleanup_box_ids: Default::default(),
        cleanup_names: Default::default(),
        lease_retirements: Default::default(),
        source_process: None,
        source_stop_confirmed: false,
        start_attempted: false,
        started_process: None,
        boot_executable: None,
        boot_guard: None,
        lease_expires_unix_ms: 10000,
        staging_bytes: 1 << 20,
        snapshot_committed: false,
        verified: false,
        remote_verified: false,
        cache_until_unix_ms: 0,
        lease_ttl_sec: 0,
    }
}
fn archive() -> Archive {
    Archive {
        snapshot_id: "suspend-1".into(),
        size_bytes: 1024,
        sha256: "b".repeat(64),
        expanded_bytes: 8192,
        format: ARCHIVE_FORMAT.into(),
    }
}

#[test]
fn boot_intent_and_process_survive_reopen_and_cannot_be_replaced() {
    let root = Root::new();
    let j = root.open();
    let op = source();
    let id = op.operation_id.clone();
    j.claim(op).unwrap();
    // A lease and rollback authority are required before any native boot intent.
    assert_eq!(
        j.update(&id, |op| {
            op.start_attempted = true;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    j.update(&id, |op| {
        op.phase = Phase::RollingBack;
        op.start_attempted = true;
        Ok(())
    })
    .unwrap();
    j.close().unwrap();
    let j = root.open();
    assert!(j.snapshot().unwrap().operations[&id].start_attempted);
    assert_eq!(
        j.update(&id, |op| {
            op.start_attempted = false;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let process = crate::persistence::HostProcess::capture(std::process::id()).unwrap();
    j.update(&id, |op| {
        op.started_process = Some(process.clone());
        Ok(())
    })
    .unwrap();
    j.close().unwrap();
    let j = root.open();
    assert!(
        j.snapshot().unwrap().operations[&id]
            .started_process
            .as_ref()
            == Some(&process)
    );
    assert_eq!(
        j.update(&id, |op| {
            op.started_process = None;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let mut replacement = process;
    replacement.start_time += 1;
    assert_eq!(
        j.update(&id, |op| {
            op.started_process = Some(replacement);
            Ok(())
        }),
        Err(Error::Conflict)
    );
    j.update(&id, |op| {
        op.phase = Phase::RolledBack;
        Ok(())
    })
    .unwrap();
}
fn target(generation: i64) -> Operation {
    let mut op = source();
    op.operation_id = format!("resume-{generation}");
    op.generation = generation;
    op.kind = Kind::Target;
    op.phase = Phase::Prepared;
    op.box_id.clear();
    op.lease_expires_unix_ms = 0;
    op.archive = Some(archive());
    op.lease_ttl_sec = 300;
    op.artifact = artifact_name(&op.scoped_session_id, &op.operation_id, generation);
    op
}
fn deletion() -> Operation {
    let mut op = source();
    op.operation_id = "delete-session".into();
    op.generation = 2;
    op.kind = Kind::Delete;
    op.phase = Phase::Deleting;
    op.lease_expires_unix_ms = 0;
    op.staging_bytes = 0;
    op
}
fn freeze(j: &Journal) {
    j.update("suspend-1", |op| {
        op.source_process = Some(crate::persistence::HostProcess {
            host_id: "00000000-0000-4000-8000-000000000002".into(),
            boot_id: "00000000-0000-4000-8000-000000000001".into(),
            process_namespace: "host".into(),
            pid: 1234,
            start_time: 10,
            writer_cgroup: None,
            signal_version: None,
        });
        op.source_stop_confirmed = true;
        op.phase = Phase::Frozen;
        op.archive = Some(archive());
        op.verified = true;
        op.artifact = artifact_name(&op.scoped_session_id, &op.operation_id, op.generation);
        Ok(())
    })
    .unwrap();
}
fn commit(j: &Journal) {
    j.update("suspend-1", |op| {
        op.remote_verified = true;
        op.snapshot_committed = true;
        op.phase = Phase::Committed;
        Ok(())
    })
    .unwrap();
    j.update("suspend-1", |op| {
        op.phase = Phase::Finalized;
        Ok(())
    })
    .unwrap();
}

#[test]
fn cleanup_evidence_and_name_reservations_are_durable_and_monotonic() {
    let root = Root::new();
    let j = root.open();
    j.claim(source()).unwrap();
    freeze(&j);
    commit(&j);
    let name = crate::persistence::terminal_name("obx:owner:session");
    let process = crate::persistence::HostProcess::capture(std::process::id()).unwrap();
    j.update("suspend-1", |op| {
        op.cleanup_box_ids.insert(op.box_id.clone());
        op.cleanup_names.insert(op.box_id.clone(), name.clone());
        op.cleanup_processes
            .insert(op.box_id.clone(), process.clone());
        op.name_fences.insert(name.clone(), "marker_123".into());
        Ok(())
    })
    .unwrap();
    j.close().unwrap();
    let j = root.open();
    let op = j.snapshot().unwrap().operations["suspend-1"].clone();
    assert_eq!(op.name_fences[&name], "marker_123");
    assert!(op.cleanup_processes[&op.box_id] == process);
    for field in ["ids", "idnames", "processes", "names", "replacement"] {
        assert_eq!(
            j.update("suspend-1", |op| {
                match field {
                    "ids" => op.cleanup_box_ids.clear(),
                    "idnames" => op.cleanup_names.clear(),
                    "processes" => op.cleanup_processes.clear(),
                    "names" => op.name_fences.clear(),
                    _ => {
                        op.name_fences.insert(name.clone(), "replacement".into());
                    }
                }
                Ok(())
            }),
            Err(Error::Conflict)
        );
    }
    let mut unrelated = source();
    unrelated.cleanup_box_ids.insert("unknown_native".into());
    assert_eq!(unrelated.validate(), Err(Error::Conflict));
}
fn ready(j: &Journal, id: &str) {
    j.update(id, |op| {
        op.phase = Phase::Importing;
        op.verified = true;
        Ok(())
    })
    .unwrap();
    j.update(id, |op| {
        op.box_id = "target_box_123".into();
        op.phase = Phase::Ready;
        op.lease_expires_unix_ms = 20000;
        Ok(())
    })
    .unwrap();
}

#[test]
fn private_files_owner_lock_and_nested_directories() {
    let root = Root::new();
    let path = root.journal().join("nested");
    let j = Journal::open(&path, "worker-a").unwrap();
    for name in ["journal.json", "owner.json", "journal.lock"] {
        assert_eq!(
            fs::metadata(path.join(name)).unwrap().permissions().mode() & 0o777,
            0o600
        );
    }
    for dir in [&path, &path.join("artifacts")] {
        assert_eq!(
            fs::metadata(dir).unwrap().permissions().mode() & 0o777,
            0o700
        );
    }
    assert!(matches!(
        Journal::open(&path, "worker-a"),
        Err(Error::Unavailable)
    ));
    j.close().unwrap();
    assert!(matches!(
        Journal::open(&path, "worker-b"),
        Err(Error::Unavailable)
    ));
    let reopened = Journal::open(&path, "worker-a").unwrap();
    assert_eq!(reopened.snapshot().unwrap().worker_id, "worker-a");
}

#[test]
fn explicit_close_releases_the_lock_even_if_a_descriptor_alias_survives() {
    let root = Root::new();
    let journal = root.open();
    // A concurrent fork can inherit the open file description briefly before
    // CLOEXEC closes it. An alias reproduces that ownership deterministically.
    let alias = journal
        .inner
        .lock()
        .unwrap()
        .lock
        .as_ref()
        .unwrap()
        .try_clone()
        .unwrap();
    journal.close().unwrap();
    assert_eq!(journal.snapshot().err(), Some(Error::Unavailable));
    let replacement = root.open();
    assert!(Journal::open(root.journal(), "worker-a").is_err());
    drop(alias);
    assert!(Journal::open(root.journal(), "worker-a").is_err());
    replacement.snapshot().unwrap();
}

#[test]
fn reopen_preserves_source_identity_spec_and_commit_proof() {
    let root = Root::new();
    let j = root.open();
    let op = source();
    j.claim(op.clone()).unwrap();
    freeze(&j);
    commit(&j);
    j.close().unwrap();
    let j = root.open();
    let state = j.snapshot().unwrap();
    let saved = &state.operations[&op.operation_id];
    assert_eq!(saved.phase, Phase::Finalized);
    assert!(saved.restore_spec == op.restore_spec);
    assert_eq!(saved.box_id, op.box_id);
    assert!(saved.remote_verified && saved.snapshot_committed && saved.verified);
    assert!(saved.archive == Some(archive()));
    assert_eq!(
        j.update(&op.operation_id, |op| {
            op.phase = Phase::Frozen;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    j.claim(target(2)).unwrap();
    assert_eq!(j.claim(op), Err(Error::Conflict));
}

#[test]
fn commits_require_verified_archive_and_remote_readback() {
    let root = Root::new();
    let j = root.open();
    j.claim(source()).unwrap();
    assert_eq!(
        j.update("suspend-1", |op| {
            op.phase = Phase::Frozen;
            op.archive = Some(archive());
            op.artifact = artifact_name(&op.scoped_session_id, &op.operation_id, 1);
            Ok(())
        }),
        Err(Error::Conflict)
    );
    freeze(&j);
    assert_eq!(
        j.update("suspend-1", |op| {
            op.phase = Phase::Committed;
            op.snapshot_committed = true;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    assert_eq!(
        j.snapshot().unwrap().operations["suspend-1"].phase,
        Phase::Frozen
    );
    commit(&j);
}

#[test]
fn update_cannot_change_chosen_identity_spec_archive_or_lease() {
    let root = Root::new();
    let j = root.open();
    let op = target(2);
    let id = op.operation_id.clone();
    j.claim(op).unwrap();
    ready(&j, &id);
    let mutations: Vec<fn(&mut Operation)> = vec![
        |op| op.generation += 1,
        |op| op.scoped_session_id = "obx:owner:other".into(),
        |op| op.operation_id = "other".into(),
        |op| op.box_id = "replacement".into(),
        |op| op.restore_spec.as_mut().unwrap().image = format!("other@sha256:{}", "c".repeat(64)),
        |op| op.archive.as_mut().unwrap().sha256 = "d".repeat(64),
        |op| op.lease_expires_unix_ms -= 1,
        |op| op.staging_bytes += 1,
        |op| op.lease_ttl_sec += 1,
        |op| op.artifact = "../outside".into(),
    ];
    for mutation in mutations {
        assert_eq!(
            j.update(&id, |op| {
                mutation(op);
                Ok(())
            }),
            Err(Error::Conflict)
        );
    }
    j.update(&id, |op| {
        op.lease_expires_unix_ms += 100;
        op.phase = Phase::Active;
        Ok(())
    })
    .unwrap();
    assert_eq!(
        j.update(&id, |op| {
            op.phase = Phase::Ready;
            Ok(())
        }),
        Err(Error::Conflict)
    );
}

#[test]
fn verification_flags_cannot_be_forgotten() {
    let root = Root::new();
    let j = root.open();
    j.claim(source()).unwrap();
    freeze(&j);
    j.update("suspend-1", |op| {
        op.remote_verified = true;
        Ok(())
    })
    .unwrap();
    assert_eq!(
        j.update("suspend-1", |op| {
            op.remote_verified = false;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    assert_eq!(
        j.update("suspend-1", |op| {
            op.verified = false;
            Ok(())
        }),
        Err(Error::Conflict)
    );
}

#[test]
fn identical_claim_replays_without_rewinding_progress() {
    let root = Root::new();
    let j = root.open();
    let op = source();
    j.claim(op.clone()).unwrap();
    freeze(&j);
    j.claim(op.clone()).unwrap();
    assert_eq!(
        j.snapshot().unwrap().operations["suspend-1"].phase,
        Phase::Frozen
    );
    let mut different = op;
    different.box_id = "different-box".into();
    assert_eq!(j.claim(different), Err(Error::Conflict));
}

#[test]
fn target_replay_cannot_substitute_another_archive() {
    let root = Root::new();
    let j = root.open();
    let mut op = target(2);
    j.claim(op.clone()).unwrap();
    ready(&j, &op.operation_id);
    j.claim(op.clone()).unwrap();
    op.archive.as_mut().unwrap().sha256 = "c".repeat(64);
    assert_eq!(j.claim(op), Err(Error::Conflict));
}

#[test]
fn generation_and_phase_fences_survive_cancellation() {
    let root = Root::new();
    let j = root.open();
    let op = target(2);
    let id = op.operation_id.clone();
    j.claim(op).unwrap();
    ready(&j, &id);
    assert_eq!(j.claim(target(3)), Err(Error::Conflict));
    j.update(&id, |op| {
        op.phase = Phase::Canceling;
        Ok(())
    })
    .unwrap();
    j.update(&id, |op| {
        op.phase = Phase::Canceled;
        Ok(())
    })
    .unwrap();
    assert_eq!(
        j.update(&id, |op| {
            op.phase = Phase::Importing;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    j.close().unwrap();
    let j = root.open();
    assert_eq!(j.claim(target(2)), Ok(())); // A replay reports the canceled record.
    assert_eq!(j.snapshot().unwrap().operations[&id].phase, Phase::Canceled);
    j.claim(target(3)).unwrap();
    assert_eq!(j.claim(target(2)), Err(Error::Conflict));
}

#[test]
fn deletion_is_permanent_and_has_an_immutable_native_fence() {
    let root = Root::new();
    let j = root.open();
    j.claim(source()).unwrap();
    j.claim(deletion()).unwrap();
    assert_eq!(
        j.update("delete-session", |op| {
            op.phase = Phase::Deleted;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    j.update("delete-session", |op| {
        op.tombstone_box_id = "tombstone_123".into();
        op.phase = Phase::Deleted;
        Ok(())
    })
    .unwrap();
    j.close().unwrap();
    let j = root.open();
    assert_eq!(j.claim(target(3)), Err(Error::Conflict));
    assert_eq!(
        j.cancel_unknown("late-cancel", "obx:owner:session", 3),
        Err(Error::Conflict)
    );
    assert_eq!(j.claim(target(4)), Err(Error::Conflict));
    assert_eq!(
        j.update("suspend-1", |op| {
            op.phase = Phase::RollingBack;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    assert_eq!(
        j.update("delete-session", |op| {
            op.tombstone_box_id = "replacement".into();
            Ok(())
        }),
        Err(Error::Conflict)
    );
}

#[test]
fn adoption_and_rollback_keep_the_same_box() {
    let root = Root::new();
    let j = root.open();
    let mut adopted = source();
    adopted.operation_id = "adopt".into();
    adopted.kind = Kind::Adopt;
    adopted.phase = Phase::Active;
    adopted.staging_bytes = 0;
    j.claim(adopted).unwrap();
    let mut wrong = source();
    wrong.box_id = "wrong-box".into();
    assert_eq!(j.claim(wrong), Err(Error::Conflict));
    j.claim(source()).unwrap();
    j.update("suspend-1", |op| {
        op.phase = Phase::RollingBack;
        Ok(())
    })
    .unwrap();
    j.update("suspend-1", |op| {
        op.phase = Phase::RolledBack;
        Ok(())
    })
    .unwrap();
    let mut next = source();
    next.operation_id = "suspend-2".into();
    j.claim(next).unwrap();
}

#[test]
fn unknown_cancellation_preserves_active_owner_and_cancels_future_replay() {
    let root = Root::new();
    let j = root.open();
    let op = target(2);
    let id = op.operation_id.clone();
    j.claim(op).unwrap();
    ready(&j, &id);
    j.update(&id, |op| {
        op.phase = Phase::Active;
        Ok(())
    })
    .unwrap();
    j.cancel_unknown("lost-resume", "obx:owner:session", 3)
        .unwrap();
    let state = j.snapshot().unwrap();
    assert_eq!(state.current["obx:owner:session"], id);
    assert_eq!(state.operations["lost-resume"].phase, Phase::Canceled);
    let mut replay = target(3);
    replay.operation_id = "lost-resume".into();
    replay.artifact = artifact_name(&replay.scoped_session_id, &replay.operation_id, 3);
    assert_eq!(j.claim(replay), Err(Error::Conflict));
}

#[test]
fn unknown_cancellation_fences_an_unclaimed_worker_after_restart() {
    let root = Root::new();
    let j = root.open();
    j.cancel_unknown("resume-2", "obx:owner:session", 2)
        .unwrap();
    j.close().unwrap();
    let j = root.open();
    assert_eq!(j.claim(target(2)), Err(Error::Conflict));
    j.claim(target(3)).unwrap();
}

#[test]
fn bootstrap_probe_pins_native_identity_across_restart() {
    let root = Root::new();
    let j = root.open();
    j.probe("obx:owner:session", "source_box_123").unwrap();
    j.close().unwrap();
    let j = root.open();
    assert_eq!(
        j.probe("obx:owner:session", "other-box"),
        Err(Error::Conflict)
    );
    let mut wrong = source();
    wrong.box_id = "other-box".into();
    assert_eq!(j.claim(wrong), Err(Error::Conflict));
    j.claim(source()).unwrap();
    freeze(&j);
    commit(&j);
    j.claim(target(2)).unwrap();
    assert!(j.snapshot().unwrap().probes.is_empty());
}

#[test]
fn concurrent_target_claims_choose_one_owner() {
    let root = Root::new();
    let j = Arc::new(root.open());
    let barrier = Arc::new(Barrier::new(8));
    let threads: Vec<_> = (0..8)
        .map(|i| {
            let j = j.clone();
            let barrier = barrier.clone();
            std::thread::spawn(move || {
                let mut op = target(2);
                op.operation_id = format!("candidate-{i}");
                op.artifact = artifact_name(&op.scoped_session_id, &op.operation_id, 2);
                barrier.wait();
                j.claim(op)
            })
        })
        .collect();
    assert_eq!(
        threads
            .into_iter()
            .map(|thread| thread.join().unwrap())
            .filter(Result::is_ok)
            .count(),
        1
    );
    assert_eq!(j.snapshot().unwrap().operations.len(), 1);
}

#[test]
fn snapshots_are_copies_and_operation_ids_are_global() {
    let root = Root::new();
    let j = root.open();
    j.claim(source()).unwrap();
    let mut copy = j.snapshot().unwrap();
    copy.operations.get_mut("suspend-1").unwrap().generation = 99;
    assert_eq!(j.snapshot().unwrap().operations["suspend-1"].generation, 1);
    let mut collision = source();
    collision.scoped_session_id = "obx:other:session".into();
    assert_eq!(j.claim(collision), Err(Error::Conflict));
}

#[test]
fn write_failure_poisoning_does_not_acknowledge_an_unstored_generation() {
    let root = Root::new();
    let mut j = root.open();
    j.persist = |_, _| Err(Error::Unavailable);
    assert_eq!(j.claim(source()), Err(Error::Unavailable));
    assert!(matches!(j.snapshot(), Err(Error::Unavailable)));
    assert_eq!(
        j.cancel_unknown("cancel", "obx:owner:other", 1),
        Err(Error::Unavailable)
    );
    j.close().unwrap();
    assert!(root.open().snapshot().unwrap().operations.is_empty());
}

#[test]
fn lost_commit_acknowledgement_recovers_the_new_generation_after_reopen() {
    let root = Root::new();
    let mut j = root.open();
    j.persist = |root, state| {
        persist_state(root, state)?;
        Err(Error::Unavailable)
    };
    assert_eq!(j.claim(source()), Err(Error::Unavailable));
    assert!(matches!(j.snapshot(), Err(Error::Unavailable)));
    j.close().unwrap();
    let state = root.open().snapshot().unwrap();
    assert_eq!(state.current["obx:owner:session"], "suspend-1");
}

#[test]
fn rejected_mutation_leaves_the_journal_usable() {
    let root = Root::new();
    let j = root.open();
    j.claim(source()).unwrap();
    assert_eq!(
        j.update("suspend-1", |op| {
            op.box_id = "mutated".into();
            Err(Error::Conflict)
        }),
        Err(Error::Conflict)
    );
    freeze(&j);
    assert_eq!(
        j.snapshot().unwrap().operations["suspend-1"].box_id,
        "source_box_123"
    );
}

#[test]
fn missing_journal_and_unregistered_files_never_initialize_fresh_history() {
    let root = Root::new();
    let j = root.open();
    j.claim(source()).unwrap();
    j.close().unwrap();
    fs::remove_file(root.journal().join("journal.json")).unwrap();
    assert!(matches!(
        Journal::open(root.journal(), "worker-a"),
        Err(Error::Unavailable)
    ));
    let other = root.0.join("orphaned");
    private_directory(&other).unwrap();
    fs::write(other.join("orphan.boxlite"), b"private-data").unwrap();
    assert!(matches!(
        Journal::open(other, "worker-a"),
        Err(Error::Unavailable)
    ));
}

#[test]
fn corrupt_oversized_and_unknown_private_records_fail_closed() {
    for case in [
        "truncated",
        "unknown-field",
        "missing-current",
        "oversized",
        "owner",
    ] {
        let root = Root::new();
        let j = root.open();
        j.claim(source()).unwrap();
        j.close().unwrap();
        let path = root.journal().join("journal.json");
        match case {
            "truncated" => fs::write(&path, b"{").unwrap(),
            "oversized" => OpenOptions::new()
                .write(true)
                .open(&path)
                .unwrap()
                .set_len(MAX_JOURNAL_BYTES as u64 + 1)
                .unwrap(),
            "owner" => fs::write(
                root.journal().join("owner.json"),
                br#"{"version":1,"worker_id":"other"}"#,
            )
            .unwrap(),
            _ => {
                let mut value: serde_json::Value =
                    serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
                if case == "unknown-field" {
                    value["operations"]["suspend-1"]["download"] =
                        serde_json::json!({"url":"https://private.invalid?grant=secret"});
                } else {
                    value["current"] = serde_json::json!({});
                }
                fs::write(&path, serde_json::to_vec(&value).unwrap()).unwrap();
            }
        }
        assert!(
            matches!(
                Journal::open(root.journal(), "worker-a"),
                Err(Error::Unavailable)
            ),
            "case {case}"
        );
    }
}

#[test]
fn insecure_permissions_symlinks_hardlinks_and_fifos_are_rejected() {
    for name in ["journal.json", "owner.json", "journal.lock", "artifacts"] {
        let root = Root::new();
        let j = root.open();
        j.close().unwrap();
        let path = root.journal().join(name);
        let outside = root.0.join("outside");
        fs::write(&outside, b"unchanged").unwrap();
        if name == "artifacts" {
            fs::remove_dir(&path).unwrap();
        } else {
            fs::remove_file(&path).unwrap();
        }
        symlink(&outside, &path).unwrap();
        assert!(matches!(
            Journal::open(root.journal(), "worker-a"),
            Err(Error::Unavailable)
        ));
        assert_eq!(fs::read(&outside).unwrap(), b"unchanged");
    }
    let root = Root::new();
    let j = root.open();
    j.close().unwrap();
    let path = root.journal().join("journal.json");
    fs::hard_link(&path, root.0.join("linked")).unwrap();
    assert!(matches!(
        Journal::open(root.journal(), "worker-a"),
        Err(Error::Unavailable)
    ));
    fs::remove_file(&path).unwrap();
    let name = std::ffi::CString::new(path.as_os_str().as_bytes()).unwrap();
    // SAFETY: name is a live NUL-terminated path to this test's private fixture.
    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
    assert!(matches!(
        Journal::open(root.journal(), "worker-a"),
        Err(Error::Unavailable)
    ));
    let other = root.0.join("readable");
    private_directory(&other).unwrap();
    fs::set_permissions(&other, fs::Permissions::from_mode(0o755)).unwrap();
    assert!(matches!(
        Journal::open(other, "worker-a"),
        Err(Error::Unavailable)
    ));
}

#[test]
fn artifact_paths_never_use_scope_as_a_filesystem_path() {
    let root = Root::new();
    let j = root.open();
    let path = j
        .artifact_path("obx:owner:../../outside", "operation", 7)
        .unwrap();
    assert_eq!(
        path.parent().unwrap(),
        fs::canonicalize(root.journal()).unwrap().join("artifacts")
    );
    assert_eq!(path.file_name().unwrap().to_str().unwrap().len(), 72);
    assert_eq!(
        j.artifact_path("obx:owner:session", "../escape", 1),
        Err(Error::Conflict)
    );
    assert_eq!(
        j.artifact_path("wrong-scope", "operation", 1),
        Err(Error::Conflict)
    );
}

#[test]
fn private_spec_validation_rejects_invalid_runtime_format_resources_and_digest() {
    let original = source();
    let mutations: Vec<fn(&mut RestoreSpec)> = vec![
        |s| s.runtime = "docker".into(),
        |s| s.platform = "darwin/arm64".into(),
        |s| s.image = "image:latest".into(),
        |s| s.format = "docker-rootfs-v1".into(),
        |s| s.cpus = f64::NAN,
        |s| s.cpus = 0.5,
        |s| s.cpus = 256.0,
        |s| s.memory_mib = 0,
        |s| s.memory_mib = u32::MAX as i64 + 1,
        |s| s.max_processes = 0,
        |s| {
            s.env.insert("too-big".into(), "x".repeat(33 << 10));
        },
    ];
    for mutate in mutations {
        let mut spec = original.restore_spec.clone().unwrap();
        mutate(&mut spec);
        assert_eq!(spec.validate(), Err(Error::Conflict));
    }
    assert_eq!(original.validate(), Ok(()));
}

#[test]
fn process_helper() {
    let Ok(path) = std::env::var("ONLYBOXES_JOURNAL_TEST_ROOT") else {
        return;
    };
    if std::env::var("ONLYBOXES_JOURNAL_TEST_MODE").unwrap() == "blocked" {
        assert!(matches!(
            Journal::open(path, "worker-a"),
            Err(Error::Unavailable)
        ));
        return;
    }
    let j = Journal::open(path, "worker-a").unwrap();
    j.claim(source()).unwrap();
    if std::env::var("ONLYBOXES_JOURNAL_TEST_MODE").unwrap() == "pin_source" {
        let pid = std::env::var("ONLYBOXES_JOURNAL_TEST_SOURCE_PID")
            .unwrap()
            .parse()
            .unwrap();
        let process = crate::persistence::HostProcess::capture(pid).unwrap();
        j.update("suspend-1", |op| {
            op.source_process = Some(process);
            Ok(())
        })
        .unwrap();
    }
    println!("JOURNAL_READY");
    std::io::stdout().flush().unwrap();
    loop {
        std::thread::park();
    }
}

fn helper(path: &Path, mode: &str) -> Command {
    let mut command = Command::new(std::env::current_exe().unwrap());
    command
        .args([
            "--exact",
            "persistence::journal::tests::process_helper",
            "--nocapture",
        ])
        .env("ONLYBOXES_JOURNAL_TEST_ROOT", path)
        .env("ONLYBOXES_JOURNAL_TEST_MODE", mode);
    command
}

#[test]
fn cross_process_lock_and_sigkill_recovery() {
    let root = Root::new();
    let j = root.open();
    assert!(helper(&root.journal(), "blocked")
        .status()
        .unwrap()
        .success());
    j.close().unwrap();
    let mut child = helper(&root.journal(), "hold")
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    let output = child.stdout.take().unwrap();
    let mut ready = false;
    for line in BufReader::new(output).lines() {
        if line.unwrap().contains("JOURNAL_READY") {
            ready = true;
            break;
        }
    }
    assert!(ready);
    assert!(matches!(
        Journal::open(root.journal(), "worker-a"),
        Err(Error::Unavailable)
    ));
    child.kill().unwrap();
    assert!(!child.wait().unwrap().success());
    let j = root.open();
    assert_eq!(
        j.snapshot().unwrap().operations["suspend-1"].box_id,
        "source_box_123"
    );
    freeze(&j);
}

#[test]
fn loading_cannot_rewind_generation_or_bypass_a_prior_deletion() {
    for deletion_history in [false, true] {
        let root = Root::new();
        let j = root.open();
        j.claim(source()).unwrap();
        let next = if deletion_history {
            deletion()
        } else {
            freeze(&j);
            commit(&j);
            target(2)
        };
        j.claim(next).unwrap();
        j.close().unwrap();
        let path = root.journal().join("journal.json");
        let mut value: serde_json::Value =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        value["current"]["obx:owner:session"] = serde_json::json!("suspend-1");
        fs::write(&path, serde_json::to_vec(&value).unwrap()).unwrap();
        assert!(matches!(
            Journal::open(root.journal(), "worker-a"),
            Err(Error::Unavailable)
        ));
    }
}

#[test]
fn target_cannot_begin_import_before_local_verification() {
    let root = Root::new();
    let j = root.open();
    let op = target(2);
    let id = op.operation_id.clone();
    j.claim(op).unwrap();
    assert_eq!(
        j.update(&id, |op| {
            op.phase = Phase::Importing;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    ready(&j, &id);
}

struct OwnedChild(std::process::Child);
impl Drop for OwnedChild {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[tokio::test]
async fn source_process_proof_survives_worker_sigkill_and_cannot_be_replaced() {
    use crate::persistence::{native::stop::confirm_source_exit, HostProcess};
    use std::time::Duration;
    let root = Root::new();
    let mut source_child = OwnedChild(Command::new("sleep").arg("300").spawn().unwrap());
    let expected = HostProcess::capture(source_child.0.id()).unwrap();
    let mut worker = OwnedChild(
        helper(&root.journal(), "pin_source")
            .env(
                "ONLYBOXES_JOURNAL_TEST_SOURCE_PID",
                source_child.0.id().to_string(),
            )
            .stdout(Stdio::piped())
            .spawn()
            .unwrap(),
    );
    let stdout = worker.0.stdout.take().unwrap();
    let mut ready = false;
    for line in BufReader::new(stdout).lines() {
        if line.unwrap().contains("JOURNAL_READY") {
            ready = true;
            break;
        }
    }
    assert!(ready);
    worker.0.kill().unwrap();
    assert!(!worker.0.wait().unwrap().success());
    let j = root.open();
    let saved = j.snapshot().unwrap().operations["suspend-1"].clone();
    assert_eq!(saved.source_process.as_ref(), Some(&expected));
    assert!(!saved.source_stop_confirmed);
    // Even if an SDK record says Stopped, the independent confirmation path
    // must reject a still-live writer, including after losing the Worker.
    assert_eq!(
        confirm_source_exit(&j, "suspend-1", Duration::ZERO).await,
        Err(crate::persistence::NativeError::Unavailable)
    );
    assert!(!j.snapshot().unwrap().operations["suspend-1"].source_stop_confirmed);
    assert_eq!(
        j.update("suspend-1", |op| {
            op.phase = Phase::Frozen;
            op.archive = Some(archive());
            op.verified = true;
            op.artifact = artifact_name(&op.scoped_session_id, &op.operation_id, op.generation);
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let replacement = HostProcess::capture(std::process::id()).unwrap();
    assert_eq!(
        j.update("suspend-1", |op| {
            op.source_process = Some(replacement.clone());
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let mut replay = source();
    replay.source_process = Some(replacement);
    assert_eq!(j.claim(replay), Err(Error::Conflict));
    source_child.0.kill().unwrap();
    source_child.0.wait().unwrap();
    confirm_source_exit(&j, "suspend-1", Duration::from_secs(1))
        .await
        .unwrap();
    j.close().unwrap();
    let j = root.open();
    let confirmed = &j.snapshot().unwrap().operations["suspend-1"];
    assert!(confirmed.source_stop_confirmed);
    assert_eq!(confirmed.source_process.as_ref(), Some(&expected));
    assert_eq!(
        j.update("suspend-1", |op| {
            op.source_stop_confirmed = false;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    assert_eq!(
        j.update("suspend-1", |op| {
            op.source_process = None;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    j.update("suspend-1", |op| {
        op.phase = Phase::Frozen;
        op.archive = Some(archive());
        op.verified = true;
        op.artifact = artifact_name(&op.scoped_session_id, &op.operation_id, op.generation);
        Ok(())
    })
    .unwrap();
}

#[tokio::test]
async fn stopped_metadata_without_captured_process_identity_is_insufficient() {
    let root = Root::new();
    let j = root.open();
    j.claim(source()).unwrap();
    assert_eq!(
        crate::persistence::native::stop::confirm_source_exit(
            &j,
            "suspend-1",
            std::time::Duration::ZERO
        )
        .await,
        Err(crate::persistence::NativeError::Conflict)
    );
    assert!(!j.snapshot().unwrap().operations["suspend-1"].source_stop_confirmed);
}

#[test]
fn boot_executable_is_durable_and_cannot_be_removed_or_rebound() {
    let root = Root::new();
    let home = root.0.join("native");
    private_directory(&home).unwrap();
    private_directory(&home.join("boxes")).unwrap();
    let op = source();
    private_directory(&home.join("boxes").join(&op.box_id)).unwrap();
    let executable = crate::persistence::BootExecutable::prepare(
        &home,
        &op.box_id,
        Path::new("/bin/sleep"),
        true,
    )
    .unwrap();
    let j = root.open();
    let id = op.operation_id.clone();
    j.claim(op).unwrap();
    j.update(&id, |op| {
        op.phase = Phase::RollingBack;
        op.start_attempted = true;
        op.boot_executable = Some(executable.clone());
        Ok(())
    })
    .unwrap();
    j.close().unwrap();
    let j = root.open();
    assert_eq!(
        j.snapshot().unwrap().operations[&id].boot_executable,
        Some(executable.clone())
    );
    assert_eq!(
        crate::persistence::native::preflight_home(&root.0, &j.snapshot().unwrap()),
        Err(crate::persistence::native::NativeError::Conflict)
    );
    assert_eq!(
        j.update(&id, |op| {
            op.boot_executable = None;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let mut value = serde_json::to_value(&executable).unwrap();
    value["box_id"] = "other_box".into();
    let other = serde_json::from_value(value).unwrap();
    assert_eq!(
        j.update(&id, |op| {
            op.boot_executable = Some(other);
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let mut value = serde_json::to_value(&executable).unwrap();
    value["sha256"] = "e".repeat(64).into();
    let changed = serde_json::from_value(value).unwrap();
    assert_eq!(
        j.update(&id, |op| {
            op.boot_executable = Some(changed);
            Ok(())
        }),
        Err(Error::Conflict)
    );
}

#[test]
fn companion_epoch_and_boot_authority_are_durable_and_immutable() {
    use crate::persistence::{
        BootAuthorization, BootExecutable, GuardedBoot, HostProcess, LeaseGuardIdentity,
    };
    let root = Root::new();
    let home = root.0.join("native");
    private_directory(&home).unwrap();
    private_directory(&home.join("boxes")).unwrap();
    let mut op = source();
    private_directory(&home.join("boxes").join(&op.box_id)).unwrap();
    let executable =
        BootExecutable::prepare(&home, &op.box_id, Path::new("/bin/sleep"), true).unwrap();
    let identity = LeaseGuardIdentity {
        worker_id: "worker-a".into(),
        store_id: uuid::Uuid::new_v4().to_string(),
        epoch_id: uuid::Uuid::new_v4().to_string(),
        process: HostProcess::current().unwrap(),
    };
    let binding = GuardedBoot {
        guard: identity,
        authorization: Some(BootAuthorization {
            operation_id: op.operation_id.clone(),
            box_id: op.box_id.clone(),
            scoped_session_id: op.scoped_session_id.clone(),
            generation: op.generation,
            prior_revision: None,
            expires_unix_ms: op.lease_expires_unix_ms,
            origin: HostProcess::current().unwrap(),
            executable: executable.clone(),
        }),
    };
    op.phase = Phase::RollingBack;
    op.start_attempted = true;
    op.boot_executable = Some(executable);
    op.boot_guard = Some(binding.clone());
    let id = op.operation_id.clone();
    let j = root.open();
    j.claim(source()).unwrap();
    j.update(&id, |current| {
        *current = op.clone();
        Ok(())
    })
    .unwrap();
    j.close().unwrap();
    let j = root.open();
    assert_eq!(
        j.snapshot().unwrap().operations[&id].boot_guard,
        Some(binding.clone())
    );
    assert_eq!(
        j.update(&id, |op| {
            op.boot_guard = None;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let mut changed = binding.clone();
    changed.guard.epoch_id = uuid::Uuid::new_v4().to_string();
    assert_eq!(
        j.update(&id, |op| {
            op.boot_guard = Some(changed);
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let mut changed = binding.clone();
    changed.authorization.as_mut().unwrap().origin.start_time += 1;
    assert_eq!(
        j.update(&id, |op| {
            op.boot_guard = Some(changed);
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let mut changed = binding.clone();
    changed.authorization.as_mut().unwrap().scoped_session_id = "obx:other:scope".into();
    assert_eq!(
        j.update(&id, |op| {
            op.boot_guard = Some(changed);
            Ok(())
        }),
        Err(Error::Conflict)
    );
    let mut changed = binding.clone();
    changed.guard.worker_id = "different-worker".into();
    assert!(j
        .update(&id, |op| {
            op.boot_guard = Some(changed);
            Ok(())
        })
        .is_err());
    j.claim(source()).unwrap();
    assert_eq!(
        j.snapshot().unwrap().operations[&id].boot_guard,
        Some(binding)
    );
    j.close().unwrap();
    // A valid-looking companion from another Worker cannot pass journal load.
    let path = root.journal().join("journal.json");
    let mut value: serde_json::Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    value["operations"][&id]["boot_guard"]["guard"]["worker_id"] = "different-worker".into();
    fs::write(&path, serde_json::to_vec(&value).unwrap()).unwrap();
    assert!(Journal::open(root.journal(), "worker-a").is_err());
}

#[test]
fn cleanup_receipts_are_immutable_and_a_higher_delete_retains_the_original_anchor() {
    use crate::persistence::{GuardedRetirement, HostProcess, LeaseGuardIdentity, LeaseRetirement};
    let root = Root::new();
    let j = root.open();
    let initial = source();
    let id = initial.box_id.clone();
    j.claim(initial).unwrap();
    freeze(&j);
    j.update("suspend-1", |op| {
        op.remote_verified = true;
        op.snapshot_committed = true;
        op.phase = Phase::Committed;
        Ok(())
    })
    .unwrap();
    let op = j.snapshot().unwrap().operations["suspend-1"].clone();
    let process = op.source_process.unwrap();
    let mut helper = HostProcess::current().unwrap();
    helper.host_id = process.host_id.clone();
    let receipt = GuardedRetirement {
        guard: LeaseGuardIdentity {
            worker_id: "worker-a".into(),
            store_id: uuid::Uuid::new_v4().to_string(),
            epoch_id: uuid::Uuid::new_v4().to_string(),
            process: helper,
        },
        retirement: LeaseRetirement {
            operation_id: "suspend-1".into(),
            cleanup_generation: 1,
            box_id: id.clone(),
            scoped_session_id: op.scoped_session_id.clone(),
            generation: 1,
            revision: 2,
            process,
        },
    };
    j.update("suspend-1", |op| {
        op.cleanup_box_ids.insert(id.clone());
        op.cleanup_names.insert(
            id.clone(),
            crate::persistence::terminal_name(&op.scoped_session_id),
        );
        op.lease_retirements.insert(id.clone(), receipt.clone());
        Ok(())
    })
    .unwrap();
    j.close().unwrap();
    let j = root.open();
    assert_eq!(
        j.snapshot().unwrap().operations["suspend-1"].lease_retirements[&id],
        receipt
    );
    assert_eq!(
        j.update("suspend-1", |op| {
            op.lease_retirements.clear();
            Ok(())
        }),
        Err(Error::Conflict)
    );
    assert_eq!(
        j.update("suspend-1", |op| {
            op.lease_retirements
                .get_mut(&id)
                .unwrap()
                .retirement
                .revision += 1;
            Ok(())
        }),
        Err(Error::Conflict)
    );
    j.claim(deletion()).unwrap();
    j.update("delete-session", |op| {
        op.cleanup_box_ids.insert(id.clone());
        op.lease_retirements.insert(id.clone(), receipt.clone());
        Ok(())
    })
    .unwrap();
    assert_eq!(
        j.snapshot().unwrap().operations["delete-session"].lease_retirements[&id],
        receipt
    );
    j.close().unwrap();
    let path = root.journal().join("journal.json");
    let mut state: serde_json::Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    state["operations"]["delete-session"]["lease_retirements"][&id]["retirement"]["operation_id"] =
        "unknown-origin".into();
    fs::write(path, serde_json::to_vec(&state).unwrap()).unwrap();
    assert!(Journal::open(root.journal(), "worker-a").is_err());
}
