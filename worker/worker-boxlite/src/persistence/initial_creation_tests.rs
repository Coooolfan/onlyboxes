use super::*;
use crate::persistence::{
    BootAuthorization, BootExecutable, HostProcess, InitialBoot, InitialCreation,
    LeaseGuardIdentity,
};

fn creation() -> InitialCreation {
    let process = HostProcess::current().unwrap();
    InitialCreation {
        operation_id: "initial-create-intent".into(),
        scoped_session_id: source().scoped_session_id,
        guard: LeaseGuardIdentity {
            worker_id: "worker-a".into(),
            store_id: uuid::Uuid::new_v4().to_string(),
            epoch_id: uuid::Uuid::new_v4().to_string(),
            process: process.clone(),
        },
        origin: process,
        expires_unix_ms: 10000,
        options_sha256: InitialCreation::options_digest(&boxlite::BoxOptions::default()).unwrap(),
        box_id: String::new(),
        abandoned: false,
        cleanup_complete: false,
    }
}

#[test]
fn initial_creation_is_durable_before_native_id_and_does_not_grant_execution() {
    let root = Root::new();
    let j = root.open();
    let intent = creation();
    j.begin_initial_creation(intent.clone()).unwrap();
    j.begin_initial_creation(intent.clone()).unwrap();
    let original = fs::read(root.journal().join("journal.json")).unwrap();
    assert!(j.claim(source()).is_err());
    assert!(j
        .probe(&intent.scoped_session_id, "unselected-root")
        .is_err());
    assert_eq!(
        fs::read(root.journal().join("journal.json")).unwrap(),
        original
    );
    drop(j);
    let j = root.open();
    let state = j.snapshot().unwrap();
    assert_eq!(state.initial_creations[&intent.scoped_session_id], intent);
    assert!(state.probes.is_empty() && state.initial_boots.is_empty());
    let empty_home = root.0.join("empty-sdk");
    private_directory(&empty_home).unwrap();
    assert_eq!(
        crate::persistence::native::preflight_home(&empty_home, &state),
        Err(crate::persistence::NativeError::Conflict)
    );
    // Even an empty SDK home is not a receipt that the recorded create failed.
    assert!(fs::read_dir(empty_home).unwrap().next().is_none());
    assert_eq!(
        fs::read(root.journal().join("journal.json")).unwrap(),
        original
    );
}

#[test]
fn initial_creation_selection_is_immutable_and_boot_binds_the_same_intent() {
    let root = Root::new();
    let j = root.open();
    let intent = creation();
    j.begin_initial_creation(intent.clone()).unwrap();
    for field in ["operation", "expiry", "options", "origin", "guard"] {
        let mut changed = intent.clone();
        match field {
            "operation" => changed.operation_id.push('x'),
            "expiry" => changed.expires_unix_ms += 1,
            "options" => changed.options_sha256 = "b".repeat(64),
            "origin" => changed.origin.start_time += 1,
            "guard" => changed.guard.epoch_id = uuid::Uuid::new_v4().to_string(),
            _ => unreachable!(),
        }
        assert!(j.begin_initial_creation(changed.clone()).is_err());
        assert!(j.select_initial_creation(&changed, "first-root").is_err());
    }
    j.select_initial_creation(&intent, "first-root").unwrap();
    j.select_initial_creation(&intent, "first-root").unwrap();
    assert!(j
        .select_initial_creation(&intent, "replacement-root")
        .is_err());
    assert!(j.begin_initial_creation(intent.clone()).is_err());
    let home = root.0.join("native");
    private_directory(&home).unwrap();
    private_directory(&home.join("boxes")).unwrap();
    private_directory(&home.join("boxes/first-root")).unwrap();
    let boot = InitialBoot {
        guard: intent.guard.clone(),
        managed: false,
        authorization: BootAuthorization {
            operation_id: intent.operation_id.clone(),
            scoped_session_id: intent.scoped_session_id.clone(),
            box_id: "first-root".into(),
            origin: intent.origin.clone(),
            expires_unix_ms: intent.expires_unix_ms,
            generation: 1,
            prior_revision: None,
            executable: BootExecutable::prepare(&home, "first-root", Path::new("/bin/sleep"), true)
                .unwrap(),
        },
    };
    let mut changed = boot.clone();
    changed.authorization.operation_id.push('x');
    assert!(j.begin_initial_boot(changed).is_err());
    j.begin_initial_boot(boot.clone()).unwrap();
    j.begin_initial_boot(boot.clone()).unwrap();
    drop(j);
    let j = root.open();
    let state = j.snapshot().unwrap();
    assert_eq!(state.initial_boots[&intent.scoped_session_id], boot);
    assert_eq!(
        state.initial_creations[&intent.scoped_session_id].box_id,
        "first-root"
    );
    assert_eq!(state.probes[&intent.scoped_session_id], "first-root");
    let before = fs::read(root.journal().join("journal.json")).unwrap();
    assert!(j
        .transaction(|state| {
            state
                .initial_boots
                .get_mut(&intent.scoped_session_id)
                .unwrap()
                .authorization
                .expires_unix_ms += 1;
            Ok(())
        })
        .is_err());
    assert_eq!(
        fs::read(root.journal().join("journal.json")).unwrap(),
        before
    );
    j.close().unwrap();
    let path = root.journal().join("journal.json");
    let mut stored: serde_json::Value = serde_json::from_slice(&before).unwrap();
    stored["initial_creations"][&intent.scoped_session_id]["box_id"] = "replacement-root".into();
    fs::write(path, serde_json::to_vec(&stored).unwrap()).unwrap();
    assert!(Journal::open(root.journal(), "worker-a").is_err());
}

#[test]
fn initial_creation_cannot_borrow_another_scope_native_id_or_operation() {
    let root = Root::new();
    let j = root.open();
    let first = creation();
    j.begin_initial_creation(first.clone()).unwrap();
    j.select_initial_creation(&first, "first-root").unwrap();
    let mut other = first.clone();
    other.scoped_session_id = "obx:another:session".into();
    assert!(j.begin_initial_creation(other.clone()).is_err());
    other.operation_id = "another-create".into();
    j.begin_initial_creation(other.clone()).unwrap();
    let before = fs::read(root.journal().join("journal.json")).unwrap();
    assert!(j.select_initial_creation(&other, "first-root").is_err());
    assert_eq!(
        fs::read(root.journal().join("journal.json")).unwrap(),
        before
    );
    j.select_initial_creation(&other, "another-root").unwrap();
}

#[test]
fn creation_digest_binds_sdk_normalized_limits_and_survives_config_roundtrip() {
    let mut options = boxlite::BoxOptions {
        disk_size_gb: Some(4),
        auto_delete: Some(0),
        detach: true,
        ..Default::default()
    };
    let requested = InitialCreation::options_digest(&options).unwrap();
    options.sanitize().unwrap();
    let effective = InitialCreation::options_digest(&options).unwrap();
    assert_ne!(requested, effective);
    options.sanitize().unwrap();
    assert_eq!(
        InitialCreation::options_digest(&options).unwrap(),
        effective
    );
    let stored: boxlite::BoxOptions =
        serde_json::from_slice(&serde_json::to_vec(&options).unwrap()).unwrap();
    assert_eq!(InitialCreation::options_digest(&stored).unwrap(), effective);
}

#[test]
fn abandoned_creation_denies_original_start_and_cannot_change_a_sealed_empty_selection() {
    let root = Root::new();
    let j = root.open();
    let intent = creation();
    j.begin_initial_creation(intent.clone()).unwrap();
    let expected = BTreeMap::from([(intent.scoped_session_id.clone(), intent.clone())]);
    j.abandon_initial_creations(&expected, &BTreeMap::new())
        .unwrap();
    let abandoned = j.snapshot().unwrap().initial_creations[&intent.scoped_session_id].clone();
    assert!(abandoned.abandoned && !abandoned.cleanup_complete && abandoned.box_id.is_empty());
    assert!(j.begin_initial_creation(intent.clone()).is_err());
    assert!(j.select_initial_creation(&intent, "late-root").is_err());
    let before = fs::read(root.journal().join("journal.json")).unwrap();
    assert!(j
        .abandon_initial_creations(
            &BTreeMap::from([(intent.scoped_session_id.clone(), abandoned.clone())]),
            &BTreeMap::from([(intent.scoped_session_id.clone(), "late-root".into())])
        )
        .is_err());
    assert!(j.claim(source()).is_err());
    assert_eq!(
        fs::read(root.journal().join("journal.json")).unwrap(),
        before
    );
    j.complete_initial_creation_cleanup(&abandoned).unwrap();
    drop(j);
    let j = root.open();
    let complete = j.snapshot().unwrap().initial_creations[&intent.scoped_session_id].clone();
    assert!(complete.abandoned && complete.cleanup_complete && complete.box_id.is_empty());
    assert!(j
        .abandon_initial_creations(
            &BTreeMap::from([(intent.scoped_session_id.clone(), complete.clone())]),
            &BTreeMap::new()
        )
        .is_err());
    assert!(j.begin_initial_creation(intent).is_err());
    assert!(j.claim(source()).is_err());
    j.claim(target(2)).unwrap();
}

#[test]
fn creation_option_digest_binds_every_option_and_rejects_oversized_input() {
    let mut options = boxlite::BoxOptions::default();
    let before = InitialCreation::options_digest(&options).unwrap();
    options.env.push(("PRIVATE".into(), "private-value".into()));
    assert_ne!(InitialCreation::options_digest(&options).unwrap(), before);
    options.env.push(("OVERSIZED".into(), "x".repeat(64 << 10)));
    assert!(InitialCreation::options_digest(&options).is_err());
}
