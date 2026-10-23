use super::*;
use crate::persistence::journal::private_directory;
use std::fs;
use std::path::PathBuf;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        let path = PathBuf::from("/tmp").join(format!("obx-boot-{}", uuid::Uuid::new_v4()));
        private_directory(&path).unwrap();
        private_directory(&path.join("home")).unwrap();
        private_directory(&path.join("home/boxes")).unwrap();
        private_directory(&path.join("home/boxes/owned_box")).unwrap();
        // macOS kills copied platform binaries outside their protected path.
        // Sign only this private fixture copy before pinning its final bytes.
        fs::copy("/bin/sleep", path.join("fixture-sleep")).unwrap();
        #[cfg(target_os = "macos")]
        assert!(std::process::Command::new("/usr/bin/codesign")
            .args(["--force", "--sign", "-"])
            .arg(path.join("fixture-sleep"))
            .output()
            .unwrap()
            .status
            .success());
        Self(fs::canonicalize(path).unwrap())
    }
    fn ledger(&self) -> ExecutionLeaseLedger {
        ExecutionLeaseLedger::open(self.0.join("ledger"), "worker").unwrap()
    }
    fn grant(&self) -> BootAuthorization {
        BootAuthorization {
            operation_id: "restore_1".into(),
            box_id: "owned_box".into(),
            scoped_session_id: "obx:owner:session".into(),
            generation: 4,
            prior_revision: None,
            expires_unix_ms: now_ms() + 30_000,
            origin: HostProcess::capture(std::process::id()).unwrap(),
            executable: BootExecutable::prepare(
                &self.0.join("home"),
                "owned_box",
                &self.0.join("fixture-sleep"),
                true,
            )
            .unwrap(),
        }
    }
    #[cfg(target_os = "macos")]
    fn spawn(&self) -> Child {
        Child(
            std::process::Command::new(self.0.join("home/boxes/owned_box/bin/boxlite-shim"))
                .arg("30")
                .spawn()
                .unwrap(),
        )
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}
#[cfg(target_os = "macos")]
struct Child(std::process::Child);
#[cfg(target_os = "macos")]
impl Child {
    fn process(&self) -> HostProcess {
        let mut process = HostProcess::capture(self.0.id()).unwrap();
        process.bind_writer_group("owned_box").unwrap();
        process
    }
    fn stopped(&mut self) {
        let end = Instant::now() + Duration::from_secs(3);
        while self.0.try_wait().unwrap().is_none() {
            assert!(Instant::now() < end, "boot writer was not stopped");
            std::thread::sleep(Duration::from_millis(10));
        }
    }
}
#[cfg(target_os = "macos")]
impl Drop for Child {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[test]
fn unbound_grant_is_durable_immutable_and_empty_discovery_never_confirms_stop() {
    let root = Root::new();
    let ledger = root.ledger();
    let grant = root.grant();
    assert_eq!(ledger.prepare_boot(grant.clone()).unwrap(), grant);
    assert_eq!(ledger.prepare_boot(grant.clone()).unwrap(), grant);
    assert_eq!(
        ledger.boot_status("owned_box").unwrap(),
        Some(grant.clone())
    );
    let mut altered = grant.clone();
    altered.expires_unix_ms += 1;
    assert_eq!(ledger.prepare_boot(altered), Err(Error::Conflict));
    assert_eq!(ledger.enforce_boots().unwrap(), LeaseEnforcement::default());
    assert!(!ledger.writers_stopped().unwrap());
    let id = ledger.store_id().to_owned();
    drop(ledger);
    let reopened = ExecutionLeaseLedger::open_bound(root.0.join("ledger"), "worker", &id).unwrap();
    assert_eq!(
        reopened.boot_status("owned_box").unwrap(),
        Some(grant.clone())
    );
    assert_eq!(reopened.prepare_boot(grant), Err(Error::Conflict));
    assert!(!reopened.writers_stopped().unwrap());
}

#[test]
fn boot_identity_and_scope_are_checked_before_acknowledgement() {
    let root = Root::new();
    let ledger = root.ledger();
    let grant = root.grant();
    let mut invalid = grant.clone();
    invalid.box_id = "different_box".into();
    assert_eq!(ledger.prepare_boot(invalid), Err(Error::Conflict));
    let mut invalid = grant.clone();
    invalid.origin.start_time += 1;
    assert!(ledger.prepare_boot(invalid).is_err());
    let mut invalid = grant.clone();
    invalid.prior_revision = Some(1);
    assert_eq!(ledger.prepare_boot(invalid), Err(Error::Conflict));
    ledger.prepare_boot(grant.clone()).unwrap();
    let mut conflicting = grant;
    conflicting.operation_id = "restore_2".into();
    assert_eq!(ledger.prepare_boot(conflicting), Err(Error::Conflict));
    assert!(ledger.status("owned_box").unwrap().is_none());
}

#[cfg(target_os = "macos")]
#[test]
fn pending_boot_is_stopped_without_pid_file_or_ledger_lock_and_keeps_unknown_outcome() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut grant = root.grant();
    grant.expires_unix_ms = now_ms() + 300;
    let expiry = grant.expires_unix_ms;
    ledger.prepare_boot(grant).unwrap();
    let mut child = root.spawn();
    assert!(!root.0.join("home/boxes/owned_box/shim.pid").exists());
    ledger.enforce_boots().unwrap();
    // Make further executable discovery unavailable. A known writer's deadline
    // still runs on the disk-independent expiry path, including slow mutations.
    fs::rename(
        root.0.join("home/boxes/owned_box/bin"),
        root.0.join("saved-bin"),
    )
    .unwrap();
    let _mutation = ledger.state.lock().unwrap();
    assert!(child.0.try_wait().unwrap().is_none());
    while now_ms() <= expiry {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(ledger.enforce().unwrap().attempted, 1);
    child.stopped();
    assert!(!ledger.writers_stopped().unwrap());
}

#[cfg(target_os = "macos")]
#[test]
fn binding_transfers_to_cas_renewal_and_old_boot_deadline_cannot_stop_renewed_writer() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut grant = root.grant();
    grant.expires_unix_ms = now_ms() + 1000;
    ledger.prepare_boot(grant.clone()).unwrap();
    let mut child = root.spawn();
    let process = child.process();
    let original = ledger.bind_boot(grant.clone(), process.clone()).unwrap();
    assert_eq!(original.generation, 4);
    assert_eq!(ledger.bind_boot(grant, process).unwrap(), original);
    let renewed = ExecutionLease {
        revision: 2,
        expires_unix_ms: original.expires_unix_ms + 1000,
        ..original.clone()
    };
    ledger.renew(1, renewed.clone()).unwrap();
    while now_ms() <= original.expires_unix_ms + 20 {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(ledger.enforce_boots().unwrap().attempted, 0);
    assert_eq!(ledger.enforce().unwrap().attempted, 0);
    assert!(
        child.0.try_wait().unwrap().is_none(),
        "renewed writer exited: {:?}",
        child.0.try_wait()
    );
    while now_ms() <= renewed.expires_unix_ms {
        std::thread::sleep(Duration::from_millis(10));
    }
    ledger.enforce().unwrap();
    child.stopped();
    assert!(ledger.writers_stopped().unwrap());
}

#[cfg(target_os = "macos")]
#[test]
fn rearming_requires_exact_stopped_predecessor_and_advances_its_revision() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut grant = root.grant();
    let mut original = root.spawn();
    let prior = ExecutionLease {
        box_id: grant.box_id.clone(),
        scoped_session_id: grant.scoped_session_id.clone(),
        generation: 1,
        revision: 1,
        expires_unix_ms: grant.expires_unix_ms,
        process: original.process(),
        revoked: false,
    };
    ledger.adopt(prior).unwrap();
    grant.generation = 1;
    grant.prior_revision = Some(1);
    assert_eq!(ledger.prepare_boot(grant.clone()), Err(Error::Conflict));
    original.0.kill().unwrap();
    original.stopped();
    ledger.prepare_boot(grant.clone()).unwrap();
    assert_eq!(ledger.enforce().unwrap().attempted, 0);
    let mut next = root.spawn();
    assert_eq!(ledger.enforce().unwrap().attempted, 0);
    let bound = ledger.bind_boot(grant.clone(), next.process()).unwrap();
    assert_eq!(bound.revision, 2);
    next.0.kill().unwrap();
    next.stopped();
    let mut changed_same_operation = grant.clone();
    changed_same_operation.prior_revision = Some(2);
    assert_eq!(
        ledger.prepare_boot(changed_same_operation),
        Err(Error::Conflict)
    );
    grant.operation_id = "rollback_2".into();
    assert_eq!(ledger.prepare_boot(grant.clone()), Err(Error::Conflict));
    grant.prior_revision = Some(2);
    ledger.prepare_boot(grant.clone()).unwrap();
    let last = root.spawn();
    assert_eq!(ledger.bind_boot(grant, last.process()).unwrap().revision, 3);
}

#[cfg(target_os = "macos")]
#[test]
fn bound_boot_restart_revokes_future_deadline_and_rejects_rebinding() {
    let root = Root::new();
    let ledger = root.ledger();
    let grant = root.grant();
    ledger.prepare_boot(grant.clone()).unwrap();
    let mut child = root.spawn();
    let writer = child.process();
    ledger.bind_boot(grant.clone(), writer.clone()).unwrap();
    let id = ledger.store_id().to_owned();
    drop(ledger);
    let ledger = ExecutionLeaseLedger::open_bound(root.0.join("ledger"), "worker", &id).unwrap();
    assert!(ledger.status("owned_box").unwrap().unwrap().revoked);
    assert_eq!(ledger.bind_boot(grant, writer), Err(Error::Conflict));
    assert_eq!(ledger.enforce().unwrap().attempted, 1);
    child.stopped();
    assert!(ledger.writers_stopped().unwrap());
}

#[cfg(target_os = "macos")]
#[test]
fn orphan_boot_writer_can_only_be_registered_revoked_then_retired() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut origin = Child(
        std::process::Command::new("/bin/sleep")
            .arg("30")
            .spawn()
            .unwrap(),
    );
    let mut grant = root.grant();
    grant.origin = HostProcess::capture(origin.0.id()).unwrap();
    ledger.prepare_boot(grant.clone()).unwrap();
    let mut writer = root.spawn();
    let process = writer.process();
    assert_eq!(
        ledger.revoke_boot_writer(grant.clone(), process.clone()),
        Err(Error::Conflict)
    );
    origin.0.kill().unwrap();
    origin.0.wait().unwrap();
    let mut wrong_boot = process.clone();
    wrong_boot.boot_id = uuid::Uuid::new_v4().to_string();
    assert_eq!(
        ledger.revoke_boot_writer(grant.clone(), wrong_boot),
        Err(Error::Conflict)
    );
    let revoked = ledger
        .revoke_boot_writer(grant.clone(), process.clone())
        .unwrap();
    assert!(revoked.revoked);
    assert_eq!(revoked.expires_unix_ms, grant.expires_unix_ms);
    assert_eq!(revoked.revision, 1);
    assert_eq!(
        ledger
            .revoke_boot_writer(grant.clone(), process.clone())
            .unwrap(),
        revoked
    );
    assert!(ledger.check_running("owned_box").is_err());
    assert!(ledger.bind_boot(grant.clone(), process.clone()).is_err());
    assert!(ledger
        .renew(
            1,
            ExecutionLease {
                revision: 2,
                revoked: false,
                expires_unix_ms: revoked.expires_unix_ms + 1000,
                ..revoked.clone()
            }
        )
        .is_err());
    let store_id = ledger.store_id().to_owned();
    drop(ledger);
    let ledger =
        ExecutionLeaseLedger::open_bound(root.0.join("ledger"), "worker", &store_id).unwrap();
    assert!(ledger.status("owned_box").unwrap().unwrap().revoked);
    assert!(ledger.check_running("owned_box").is_err());
    ledger.enforce().unwrap();
    writer.stopped();
    let retirement = crate::persistence::LeaseRetirement {
        operation_id: "cancel_restore".into(),
        cleanup_generation: 4,
        box_id: grant.box_id,
        scoped_session_id: grant.scoped_session_id,
        generation: revoked.generation,
        revision: revoked.revision,
        process,
    };
    ledger.prepare_retirement(retirement.clone()).unwrap();
    assert!(
        !ledger
            .retirement_status("owned_box")
            .unwrap()
            .unwrap()
            .released
    );
    ledger.finish_retirement(retirement).unwrap();
    assert!(ledger.writers_stopped().unwrap());
}

#[cfg(target_os = "macos")]
#[test]
fn orphan_boot_cleanup_uses_positive_history_but_not_empty_discovery() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut origin = Child(
        std::process::Command::new("/bin/sleep")
            .arg("30")
            .spawn()
            .unwrap(),
    );
    let mut grant = root.grant();
    grant.origin = HostProcess::capture(origin.0.id()).unwrap();
    ledger.prepare_boot(grant.clone()).unwrap();
    let mut writer = root.spawn();
    let process = writer.process();
    ledger.enforce_boots().unwrap();
    origin.0.kill().unwrap();
    origin.0.wait().unwrap();
    writer.0.kill().unwrap();
    writer.0.wait().unwrap();
    assert!(ledger.revoke_boot_writer(grant, process).unwrap().revoked);

    let empty = Root::new();
    let ledger = empty.ledger();
    let mut origin = Child(
        std::process::Command::new("/bin/sleep")
            .arg("30")
            .spawn()
            .unwrap(),
    );
    let mut grant = empty.grant();
    grant.origin = HostProcess::capture(origin.0.id()).unwrap();
    ledger.prepare_boot(grant.clone()).unwrap();
    let mut unrelated = Child(
        std::process::Command::new("/bin/sleep")
            .arg("30")
            .spawn()
            .unwrap(),
    );
    let false_pin = unrelated.process();
    unrelated.0.kill().unwrap();
    unrelated.0.wait().unwrap();
    origin.0.kill().unwrap();
    origin.0.wait().unwrap();
    assert_eq!(
        ledger.revoke_boot_writer(grant, false_pin),
        Err(Error::Conflict)
    );
    assert!(!ledger.writers_stopped().unwrap());
    assert!(ledger.status("owned_box").unwrap().is_none());
}

#[cfg(target_os = "macos")]
#[test]
fn orphan_boot_cleanup_rejects_ambiguous_writers() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut origin = Child(
        std::process::Command::new("/bin/sleep")
            .arg("30")
            .spawn()
            .unwrap(),
    );
    let mut grant = root.grant();
    grant.origin = HostProcess::capture(origin.0.id()).unwrap();
    ledger.prepare_boot(grant.clone()).unwrap();
    let first = root.spawn();
    let _second = root.spawn();
    let process = first.process();
    origin.0.kill().unwrap();
    origin.0.wait().unwrap();
    assert_eq!(
        ledger.revoke_boot_writer(grant, process),
        Err(Error::Conflict)
    );
    assert!(ledger.status("owned_box").unwrap().is_none());
    assert!(!ledger.writers_stopped().unwrap());
}

#[cfg(target_os = "macos")]
#[test]
fn orphan_boot_cleanup_preserves_the_predecessor_revision_barrier() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut grant = root.grant();
    grant.generation = 1;
    let mut original = root.spawn();
    ledger
        .adopt(ExecutionLease {
            box_id: grant.box_id.clone(),
            scoped_session_id: grant.scoped_session_id.clone(),
            generation: grant.generation,
            revision: 1,
            expires_unix_ms: grant.expires_unix_ms,
            process: original.process(),
            revoked: false,
        })
        .unwrap();
    original.0.kill().unwrap();
    original.stopped();
    let mut origin = Child(
        std::process::Command::new("/bin/sleep")
            .arg("30")
            .spawn()
            .unwrap(),
    );
    grant.origin = HostProcess::capture(origin.0.id()).unwrap();
    grant.prior_revision = Some(1);
    ledger.prepare_boot(grant.clone()).unwrap();
    let mut writer = root.spawn();
    let process = writer.process();
    assert_eq!(ledger.enforce().unwrap().attempted, 0);
    origin.0.kill().unwrap();
    origin.0.wait().unwrap();
    let revoked = ledger
        .revoke_boot_writer(grant.clone(), process.clone())
        .unwrap();
    assert_eq!(revoked.revision, 2);
    assert!(revoked.revoked);
    assert_eq!(revoked.expires_unix_ms, grant.expires_unix_ms);
    ledger.enforce().unwrap();
    writer.stopped();
}
