use super::*;
use crate::persistence::journal::private_directory;
use crate::persistence::{BootAuthorization, BootExecutable};
use std::{
    fs,
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};
struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        let path = PathBuf::from("/tmp").join(format!("obx-rt-{}", uuid::Uuid::new_v4()));
        private_directory(&path).unwrap();
        Self(fs::canonicalize(path).unwrap())
    }
    fn ledger(&self) -> ExecutionLeaseLedger {
        ExecutionLeaseLedger::open(self.0.join("ledger"), "worker").unwrap()
    }
    fn boot(&self, id: &str, generation: i64, prior_revision: Option<u64>) -> BootAuthorization {
        let home = self.0.join("home");
        private_directory(&home).unwrap();
        private_directory(&home.join("boxes")).unwrap();
        private_directory(&home.join("boxes").join(id)).unwrap();
        BootAuthorization {
            operation_id: format!("resume_{generation}"),
            box_id: id.into(),
            scoped_session_id: "obx:owner:session".into(),
            generation,
            prior_revision,
            expires_unix_ms: now() + 30_000,
            origin: HostProcess::current().unwrap(),
            executable: BootExecutable::prepare(&home, id, Path::new("/bin/sleep"), true).unwrap(),
        }
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}
#[cfg(target_os = "macos")]
struct Child(std::process::Child);
#[cfg(target_os = "macos")]
impl Child {
    fn new() -> Self {
        Self(
            std::process::Command::new("/bin/sleep")
                .arg("30")
                .spawn()
                .unwrap(),
        )
    }
    fn lease(&self, id: &str) -> ExecutionLease {
        ExecutionLease {
            box_id: id.into(),
            scoped_session_id: "obx:owner:session".into(),
            generation: 1,
            revision: 1,
            expires_unix_ms: now() + 30_000,
            process: HostProcess::inspect_writer(self.0.id(), id).unwrap(),
            revoked: false,
        }
    }
    fn stop(&mut self) {
        self.0.kill().unwrap();
        self.0.wait().unwrap();
    }
}
#[cfg(target_os = "macos")]
impl Drop for Child {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
fn receipt(lease: &ExecutionLease) -> LeaseRetirement {
    LeaseRetirement {
        operation_id: "cleanup_1".into(),
        cleanup_generation: lease.generation,
        box_id: lease.box_id.clone(),
        scoped_session_id: lease.scoped_session_id.clone(),
        generation: lease.generation,
        revision: lease.revision,
        process: lease.process.clone(),
    }
}

#[cfg(target_os = "macos")]
#[test]
fn live_stale_and_missing_owners_cannot_be_retired_or_released() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut child = Child::new();
    let lease = child.lease("owned_box");
    let retirement = receipt(&lease);
    assert_eq!(
        ledger.finish_retirement(retirement.clone()),
        Err(Error::Conflict)
    );
    assert_eq!(
        ledger.prepare_retirement(retirement.clone()),
        Err(Error::Conflict)
    );
    ledger.adopt(lease.clone()).unwrap();
    assert_eq!(
        ledger.prepare_retirement(retirement.clone()),
        Err(Error::Conflict)
    );
    let renewed = ExecutionLease {
        revision: 2,
        expires_unix_ms: lease.expires_unix_ms + 1000,
        ..lease
    };
    ledger.renew(1, renewed.clone()).unwrap();
    child.stop();
    assert_eq!(ledger.prepare_retirement(retirement), Err(Error::Conflict));
    let retirement = receipt(&renewed);
    ledger.prepare_retirement(retirement.clone()).unwrap();
    let mut different = retirement.clone();
    different.operation_id = "other_cleanup".into();
    assert_eq!(
        ledger.prepare_retirement(different.clone()),
        Err(Error::Conflict)
    );
    assert_eq!(ledger.finish_retirement(different), Err(Error::Conflict));
    assert!(
        !ledger
            .retirement_status("owned_box")
            .unwrap()
            .unwrap()
            .released
    );
}

#[cfg(target_os = "macos")]
#[test]
fn positive_exit_reserves_scope_until_explicit_release_then_only_higher_generation_can_boot() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut child = Child::new();
    let lease = child.lease("old_box");
    let retirement = receipt(&lease);
    ledger.adopt(lease).unwrap();
    child.stop();
    assert_eq!(
        ledger.prepare_retirement(retirement.clone()).unwrap(),
        retirement
    );
    assert_eq!(
        ledger.prepare_retirement(retirement.clone()).unwrap(),
        retirement
    );
    assert!(ledger.status("old_box").unwrap().unwrap().revoked);
    assert!(ledger.check_running("old_box").is_err());
    assert!(ledger.writers_stopped().unwrap());
    let target = root.boot("new_box", 2, None);
    assert_eq!(ledger.prepare_boot(target.clone()), Err(Error::Conflict));
    ledger.finish_retirement(retirement.clone()).unwrap();
    assert_eq!(
        ledger.finish_retirement(retirement.clone()).unwrap(),
        retirement
    );
    assert!(ledger.status("old_box").unwrap().is_none());
    assert!(
        ledger
            .retirement_status("old_box")
            .unwrap()
            .unwrap()
            .released
    );
    let other = Child::new();
    assert_eq!(ledger.adopt(other.lease("old_box")), Err(Error::Conflict));
    assert_eq!(
        ledger.adopt(other.lease("another_box")),
        Err(Error::Conflict)
    );
    let mut same_generation = target.clone();
    same_generation.generation = 1;
    assert_eq!(ledger.prepare_boot(same_generation), Err(Error::Conflict));
    assert_eq!(ledger.prepare_boot(target.clone()).unwrap(), target);
}

#[cfg(target_os = "macos")]
#[test]
fn retired_and_released_receipts_survive_pinned_reopen_without_rearming_writer() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut child = Child::new();
    let lease = child.lease("owned_box");
    let retirement = receipt(&lease);
    ledger.adopt(lease).unwrap();
    child.stop();
    ledger.prepare_retirement(retirement.clone()).unwrap();
    let store = ledger.store_id().to_owned();
    drop(ledger);
    let ledger = ExecutionLeaseLedger::open_bound(root.0.join("ledger"), "worker", &store).unwrap();
    assert_eq!(ledger.enforce().unwrap().attempted, 0);
    assert_eq!(
        ledger.prepare_retirement(retirement.clone()).unwrap(),
        retirement
    );
    assert!(ledger.status("owned_box").unwrap().unwrap().revoked);
    ledger.finish_retirement(retirement.clone()).unwrap();
    drop(ledger);
    let ledger = ExecutionLeaseLedger::open_bound(root.0.join("ledger"), "worker", &store).unwrap();
    assert!(
        ledger
            .retirement_status("owned_box")
            .unwrap()
            .unwrap()
            .released
    );
    assert_eq!(
        ledger.finish_retirement(retirement.clone()).unwrap(),
        retirement
    );
    assert!(ledger.status("owned_box").unwrap().is_none());
}

#[cfg(target_os = "macos")]
#[test]
fn pending_boot_cannot_be_retired_using_its_stopped_predecessor() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut child = Child::new();
    let lease = child.lease("owned_box");
    let retirement = receipt(&lease);
    ledger.adopt(lease).unwrap();
    child.stop();
    ledger
        .prepare_boot(root.boot("owned_box", 1, Some(1)))
        .unwrap();
    assert_eq!(ledger.prepare_retirement(retirement), Err(Error::Conflict));
    assert!(!ledger.writers_stopped().unwrap());
}

#[cfg(target_os = "macos")]
#[test]
fn reopening_rejects_a_pending_boot_below_the_released_generation_floor() {
    let root = Root::new();
    let ledger = root.ledger();
    let mut child = Child::new();
    let lease = child.lease("old_box");
    let retirement = receipt(&lease);
    ledger.adopt(lease).unwrap();
    child.stop();
    ledger.prepare_retirement(retirement.clone()).unwrap();
    ledger.finish_retirement(retirement).unwrap();
    ledger.prepare_boot(root.boot("new_box", 2, None)).unwrap();
    let store = ledger.store_id().to_owned();
    drop(ledger);
    let path = root.0.join("ledger/leases.json");
    let mut state: serde_json::Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    state["retirements"]["old_box"]["retirement"]["cleanup_generation"] = 2.into();
    let bytes = serde_json::to_vec(&state).unwrap();
    fs::write(&path, &bytes).unwrap();
    assert!(ExecutionLeaseLedger::open_bound(root.0.join("ledger"), "worker", &store).is_err());
    assert_eq!(fs::read(path).unwrap(), bytes);
}
