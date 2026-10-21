use super::*;
use crate::persistence::{artifact_name, Operation};
use std::os::unix::fs::PermissionsExt;

const SCOPE: &str = "obx:recovery:session";
const ID: &str = "removed_native_id";
struct Child(std::process::Child);
impl Child {
    fn new() -> Self {
        Self(
            std::process::Command::new("sleep")
                .arg("60")
                .spawn()
                .unwrap(),
        )
    }
    fn pin(&self) -> HostProcess {
        HostProcess::capture(self.0.id()).unwrap()
    }
    fn stop(&mut self) {
        self.0.kill().unwrap();
        self.0.wait().unwrap();
    }
}
impl Drop for Child {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
fn exited() -> HostProcess {
    let mut child = Child::new();
    let mut pin = child.pin();
    child.stop();
    assert!(pin.has_exited().unwrap());
    // These fixture processes are ordinary sleep children, not native Linux
    // cgroups. A previous boot provides full-writer exit evidence on Linux.
    if cfg!(target_os = "linux") {
        pin.boot_id = uuid::Uuid::new_v4().to_string();
    }
    pin
}
struct Fixture {
    root: PathBuf,
    home: PathBuf,
    journal: Journal,
}
impl Fixture {
    fn new() -> Self {
        let root = std::env::temp_dir().join(format!(
            "onlyboxes-residue-recovery-{}",
            uuid::Uuid::new_v4()
        ));
        crate::persistence::journal::private_directory(&root).unwrap();
        let home = root.join("sdk");
        for path in [&home, &home.join("db"), &home.join("boxes")] {
            crate::persistence::journal::private_directory(path).unwrap();
        }
        let home = fs::canonicalize(home).unwrap();
        let db = Connection::open(home.join("db/boxlite.db")).unwrap();
        db.execute_batch("CREATE TABLE schema_version(id INTEGER PRIMARY KEY,version INTEGER); INSERT INTO schema_version VALUES(1,10); CREATE TABLE box_config(id TEXT PRIMARY KEY,name TEXT,json TEXT); CREATE TABLE box_state(id TEXT PRIMARY KEY,status TEXT,pid INTEGER,json TEXT);").unwrap();
        let journal = Journal::open(root.join("state"), "worker").unwrap();
        Self {
            root,
            home,
            journal,
        }
    }
    fn spec() -> RestoreSpec {
        serde_json::from_value(serde_json::json!({"runtime":"boxlite","runtime_version":RUNTIME_VERSION,"platform":guest_platform().unwrap(),"format":ARCHIVE_FORMAT,
            "image":format!("image@sha256:{}","a".repeat(64)),"memory_mib":256,"cpus":1,"max_processes":128,"entrypoint":["sh"],"command":["sleep","infinity"]})).unwrap()
    }
    fn residue(&self, id: &str) -> PathBuf {
        let path = self.home.join("boxes").join(id);
        crate::persistence::journal::private_directory(&path).unwrap();
        fs::write(path.join("disk.qcow2"), b"owned residue").unwrap();
        path
    }
    fn source(&self, committed: bool, pin: HostProcess) {
        self.journal.claim(serde_json::from_value(serde_json::json!({"operation_id":"source","scoped_session_id":SCOPE,"generation":1,"kind":"source","phase":"preparing","restore_spec":Self::spec(),"box_id":ID})).unwrap()).unwrap();
        self.journal.update("source",|op| {
            op.source_process=Some(pin); op.source_stop_confirmed=true; op.phase=Phase::Frozen;
            op.archive=Some(serde_json::from_value(serde_json::json!({"snapshot_id":"source","size_bytes":1,"sha256":"a".repeat(64),"expanded_bytes":4096,"format":ARCHIVE_FORMAT})).unwrap());
            op.verified=true; op.artifact=artifact_name(SCOPE,"source",1); Ok(())
        }).unwrap();
        if committed {
            self.journal
                .update("source", |op| {
                    op.phase = Phase::Committed;
                    op.remote_verified = true;
                    op.snapshot_committed = true;
                    op.cleanup_box_ids.insert(ID.into());
                    op.cleanup_names.insert(ID.into(), terminal_name(SCOPE));
                    Ok(())
                })
                .unwrap();
        }
    }
    fn target(&self, attempted: bool, pin: Option<HostProcess>) {
        self.journal.claim(serde_json::from_value(serde_json::json!({"operation_id":"target","scoped_session_id":SCOPE,"generation":1,"kind":"target","phase":"prepared","restore_spec":Self::spec(),
            "archive":{"snapshot_id":"shared","size_bytes":1,"sha256":"a".repeat(64),"expanded_bytes":4096,"format":ARCHIVE_FORMAT},"artifact":artifact_name(SCOPE,"target",1),"staging_bytes":8192})).unwrap()).unwrap();
        self.journal
            .update("target", |op| {
                op.phase = Phase::Importing;
                op.verified = true;
                op.box_id = ID.into();
                op.start_attempted = attempted;
                op.started_process = pin;
                op.lease_expires_unix_ms = if attempted { i64::MAX } else { 0 };
                Ok(())
            })
            .unwrap();
        self.journal
            .update("target", |op| {
                op.phase = Phase::Canceling;
                op.cleanup_box_ids.insert(ID.into());
                op.cleanup_names
                    .insert(ID.into(), target_name(SCOPE, "target", 1));
                Ok(())
            })
            .unwrap();
    }
    fn pid_file(&self, id: &str, pin: &HostProcess) {
        let record = boxlite::util::PidRecord {
            pid: pin.pid,
            start_time: Some(pin.start_time),
        };
        let mut bytes = [0; 128];
        let len = record.encode(&mut bytes);
        let path = self.home.join("boxes").join(id).join("shim.pid");
        fs::write(&path, &bytes[..len]).unwrap();
        fs::set_permissions(path, fs::Permissions::from_mode(0o600)).unwrap();
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = self.journal.close();
        let _ = fs::remove_dir_all(&self.root);
    }
}

#[test]
fn committed_source_residue_is_removed_without_changing_phase_or_archive() {
    let f = Fixture::new();
    f.source(true, exited());
    let path = f.residue(ID);
    let archive = f.journal.artifact_path(SCOPE, "source", 1).unwrap();
    fs::write(&archive, b"committed archive cache").unwrap();
    let before = serde_json::to_value(f.journal.snapshot().unwrap()).unwrap();
    assert_eq!(
        super::super::preflight::preflight_home(&f.home, &f.journal.snapshot().unwrap()),
        Err(NativeError::Conflict)
    );
    reconcile_home(&f.home, &f.journal).unwrap();
    reconcile_home(&f.home, &f.journal).unwrap();
    assert!(!path.exists());
    assert_eq!(fs::read(archive).unwrap(), b"committed archive cache");
    assert_eq!(
        serde_json::to_value(f.journal.snapshot().unwrap()).unwrap(),
        before
    );
}
#[test]
fn uncommitted_or_mixed_unknown_residues_are_preserved_as_a_set() {
    let f = Fixture::new();
    f.source(false, exited());
    let path = f.residue(ID);
    assert_eq!(
        reconcile_home(&f.home, &f.journal),
        Err(NativeError::Conflict)
    );
    assert!(path.exists());
    f.journal
        .update("source", |op| {
            op.phase = Phase::Committed;
            op.remote_verified = true;
            op.snapshot_committed = true;
            Ok(())
        })
        .unwrap();
    assert_eq!(
        reconcile_home(&f.home, &f.journal),
        Err(NativeError::Conflict)
    );
    assert!(path.exists());
    f.journal
        .update("source", |op| {
            op.cleanup_box_ids.insert(ID.into());
            op.cleanup_names.insert(ID.into(), terminal_name(SCOPE));
            Ok(())
        })
        .unwrap();
    let unknown = f.residue("unknown_root");
    assert_eq!(
        reconcile_home(&f.home, &f.journal),
        Err(NativeError::Conflict)
    );
    assert!(path.exists() && unknown.exists());
    fs::remove_dir_all(unknown).unwrap();
    reconcile_home(&f.home, &f.journal).unwrap();
    assert!(!path.exists());
}
#[test]
fn live_writer_and_unknown_pid_fingerprints_block_cleanup() {
    let f = Fixture::new();
    let mut writer = Child::new();
    let pin = writer.pin();
    f.source(true, pin.clone());
    let path = f.residue(ID);
    f.pid_file(ID, &pin);
    assert_eq!(
        reconcile_home(&f.home, &f.journal),
        Err(NativeError::Conflict)
    );
    assert!(path.exists());
    writer.stop();
    let mut unknown = pin.clone();
    unknown.start_time += 1;
    f.pid_file(ID, &unknown);
    assert_eq!(
        reconcile_home(&f.home, &f.journal),
        Err(if cfg!(target_os = "linux") {
            NativeError::Unavailable
        } else {
            NativeError::Conflict
        })
    );
    assert!(path.exists());
    f.pid_file(ID, &pin);
    if cfg!(target_os = "linux") {
        assert_eq!(
            reconcile_home(&f.home, &f.journal),
            Err(NativeError::Unavailable)
        );
        assert!(path.exists());
        return;
    }
    reconcile_home(&f.home, &f.journal).unwrap();
    assert!(!path.exists());
}
#[test]
fn unstarted_import_is_safe_but_uncertain_boot_requires_recorded_cleanup_process() {
    let f = Fixture::new();
    f.target(false, None);
    let path = f.residue(ID);
    reconcile_home(&f.home, &f.journal).unwrap();
    assert!(!path.exists());
    let f = Fixture::new();
    f.target(true, None);
    let path = f.residue(ID);
    assert_eq!(
        reconcile_home(&f.home, &f.journal),
        Err(NativeError::Conflict)
    );
    assert!(path.exists());
    f.journal
        .update("target", |op| {
            op.cleanup_processes.insert(ID.into(), exited());
            Ok(())
        })
        .unwrap();
    reconcile_home(&f.home, &f.journal).unwrap();
    assert!(!path.exists());
}
#[test]
fn deletion_authorizes_uncommitted_source_only_after_writer_exit() {
    let f = Fixture::new();
    f.source(false, exited());
    let path = f.residue(ID);
    let delete:Operation=serde_json::from_value(serde_json::json!({"operation_id":"delete","scoped_session_id":SCOPE,"generation":2,"kind":"delete","phase":"deleting","restore_spec":Fixture::spec(),"box_id":ID,"cleanup_box_ids":[ID],"cleanup_names":{ID:terminal_name(SCOPE)}})).unwrap();
    f.journal.claim(delete).unwrap();
    reconcile_home(&f.home, &f.journal).unwrap();
    assert!(!path.exists());
    assert_eq!(
        f.journal.snapshot().unwrap().operations["delete"].phase,
        Phase::Deleting
    );
}

#[test]
fn every_recorded_lifecycle_must_exit_even_if_the_latest_writer_has_exited() {
    let f = Fixture::new();
    let mut old_writer = Child::new();
    f.source(false, old_writer.pin());
    let stopped = exited();
    f.journal
        .update("source", |op| {
            op.phase = Phase::RollingBack;
            op.start_attempted = true;
            op.started_process = Some(stopped.clone());
            op.lease_expires_unix_ms = i64::MAX;
            Ok(())
        })
        .unwrap();
    f.journal
        .update("source", |op| {
            op.phase = Phase::RolledBack;
            Ok(())
        })
        .unwrap();
    f.journal.claim(serde_json::from_value(serde_json::json!({"operation_id":"source-new","scoped_session_id":SCOPE,"generation":1,"kind":"source","phase":"preparing","restore_spec":Fixture::spec(),"box_id":ID})).unwrap()).unwrap();
    f.journal.update("source-new",|op| {
        op.phase=Phase::Frozen; op.source_process=Some(stopped);op.source_stop_confirmed=true;op.verified=true;
        op.archive=Some(serde_json::from_value(serde_json::json!({"snapshot_id":"source-new","size_bytes":1,"sha256":"a".repeat(64),"expanded_bytes":4096,"format":ARCHIVE_FORMAT})).unwrap());
        op.artifact=artifact_name(SCOPE,"source-new",1);Ok(())
    }).unwrap();
    f.journal
        .update("source-new", |op| {
            op.phase = Phase::Committed;
            op.remote_verified = true;
            op.snapshot_committed = true;
            op.cleanup_box_ids.insert(ID.into());
            op.cleanup_names.insert(ID.into(), terminal_name(SCOPE));
            Ok(())
        })
        .unwrap();
    let path = f.residue(ID);
    assert_eq!(
        reconcile_home(&f.home, &f.journal),
        Err(NativeError::Conflict)
    );
    assert!(path.exists());
    old_writer.stop();
    if cfg!(target_os = "linux") {
        assert_eq!(
            reconcile_home(&f.home, &f.journal),
            Err(NativeError::Unavailable)
        );
        assert!(path.exists());
        return;
    }
    reconcile_home(&f.home, &f.journal).unwrap();
    assert!(!path.exists());
}

#[test]
fn another_scopes_tombstone_reference_blocks_reclamation() {
    let f = Fixture::new();
    f.source(true, exited());
    let path = f.residue(ID);
    f.journal.claim(serde_json::from_value(serde_json::json!({
        "operation_id":"other-delete","scoped_session_id":"obx:other:session","generation":2,"kind":"delete","phase":"deleting",
        "restore_spec":Fixture::spec(),"tombstone_box_id":ID,
    })).unwrap()).unwrap();
    assert_eq!(
        reconcile_home(&f.home, &f.journal),
        Err(NativeError::Conflict)
    );
    assert_eq!(fs::read(path.join("disk.qcow2")).unwrap(), b"owned residue");
}
