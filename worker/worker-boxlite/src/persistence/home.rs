//! Ownership and census must precede the SDK's mutating startup recovery.
use super::journal::{check_file, private_directory, private_options, read_private, write_atomic};
use super::{Error, HostProcess, Journal, RUNTIME_VERSION};
use serde::{Deserialize, Serialize};
use std::fs::{self, File};
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;

type Result<T> = std::result::Result<T, Error>;
const OWNER: &str = ".onlyboxes-owner.json";
const LOCK: &str = ".onlyboxes-owner.lock";
const PAIR: &str = "native-home.json";

#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Directory {
    path: PathBuf,
    device: u64,
    inode: u64,
}
impl Directory {
    fn inspect(path: &Path) -> Result<Self> {
        let meta = fs::symlink_metadata(path).map_err(|_| Error::Unavailable)?;
        // SAFETY: geteuid reads process identity.
        if !meta.is_dir() || meta.mode() & 0o077 != 0 || meta.uid() != unsafe { libc::geteuid() } {
            return Err(Error::Unavailable);
        }
        Ok(Self {
            path: fs::canonicalize(path).map_err(|_| Error::Unavailable)?,
            device: meta.dev(),
            inode: meta.ino(),
        })
    }
}
#[derive(PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Owner {
    version: u32,
    runtime_version: String,
    worker_id: String,
    host_id: String,
    boot_id: String,
    process_namespace: String,
    pair_id: String,
    home: Directory,
    journal: Directory,
}

// Release the OFD lock on every return path, including identity/census errors.
// Closing this descriptor alone may leave an inherited CLOEXEC alias locked
// until a concurrently spawned child finishes exec.
struct HomeLock(File);
impl Drop for HomeLock {
    fn drop(&mut self) {
        let _ = self.0.unlock();
    }
}

/// Kept alive alongside the SDK runtime and its journal. This guard never
/// creates a Box, signals a process or repairs an inconsistent SDK database.
/// Boot/namespace changes currently require offline reconciliation; they are
/// never accepted by rewriting owner markers around existing native data.
pub struct RuntimeHomeGuard {
    owner: Owner,
    journal: Arc<Journal>,
    _lock: HomeLock,
}
impl RuntimeHomeGuard {
    pub fn open(
        home: impl AsRef<Path>,
        journal_root: impl AsRef<Path>,
        worker: &str,
    ) -> Result<Self> {
        Self::open_inner(home.as_ref(), journal_root.as_ref(), worker, false)
    }

    /// Startup recovery remains before SDK initialization and never starts a
    /// pending create. Only the bound live guard and exited issuer can seal
    /// an unbooted intent for subsequent configuration-only SDK cleanup.
    pub async fn open_with_execution_guard(
        home: impl AsRef<Path>,
        journal_root: impl AsRef<Path>,
        worker: &str,
        lease_state: &Path,
        client: &super::LeaseGuardClient,
    ) -> Result<Self> {
        if client.identity().worker_id != worker {
            return Err(Error::Unavailable);
        }
        let owner = Self::open_inner(home.as_ref(), journal_root.as_ref(), worker, true)?;
        client.status("initial-creation-store-check").await?;
        owner.bind_execution_store(lease_state, &client.identity().store_id)?;
        super::native::abandon_creations(&owner.owner.home.path, &owner.journal, client)
            .await
            .map_err(|_| {
                tracing::error!(
                    stage = "initial_creation_recovery",
                    "BoxLite persistence recovery check failed"
                );
                Error::Unavailable
            })?;
        super::native::reconcile_home(&owner.owner.home.path, &owner.journal).map_err(|_| {
            tracing::error!(
                stage = "native_reconciliation",
                "BoxLite persistence home check failed"
            );
            Error::Unavailable
        })?;
        Ok(owner)
    }

    fn open_inner(
        home: &Path,
        journal_root: &Path,
        worker: &str,
        defer_census: bool,
    ) -> Result<Self> {
        if !home.is_absolute()
            || !journal_root.is_absolute()
            || worker.is_empty()
            || worker.trim() != worker
            || worker.len() > 256
            || worker.chars().any(char::is_control)
        {
            return Err(Error::Unavailable);
        }
        private_directory(home)?;
        private_directory(journal_root)?;
        let home = Directory::inspect(home)?;
        let journal_dir = Directory::inspect(journal_root)?;
        if home.path.starts_with(&journal_dir.path) || journal_dir.path.starts_with(&home.path) {
            return Err(Error::Unavailable);
        }
        let lock = private_options()
            .read(true)
            .write(true)
            .create(true)
            .open(home.path.join(LOCK))
            .map_err(|_| Error::Unavailable)?;
        check_file(&lock, 0)?;
        lock.try_lock().map_err(|_| Error::Unavailable)?;
        let lock = HomeLock(lock);
        let process = HostProcess::capture(std::process::id())?;
        let saved = read_private(&home.path.join(OWNER), 8192)?;
        let (owner, journal) = if let Some(bytes) = saved {
            let owner: Owner = serde_json::from_slice(&bytes).map_err(|_| Error::Unavailable)?;
            if owner.version != 1
                || owner.runtime_version != RUNTIME_VERSION
                || owner.worker_id != worker
                || owner.home != home
                || owner.journal != journal_dir
                || owner.host_id != process.host_id
                || owner.boot_id != process.boot_id
                || owner.process_namespace != process.process_namespace
            {
                return Err(Error::Unavailable);
            }
            let id = uuid::Uuid::parse_str(&owner.pair_id).map_err(|_| Error::Unavailable)?;
            if id.is_nil() || id.to_string() != owner.pair_id {
                return Err(Error::Unavailable);
            }
            let paired: Owner = serde_json::from_slice(
                &read_private(&journal_dir.path.join(PAIR), 8192)?.ok_or(Error::Unavailable)?,
            )
            .map_err(|_| Error::Unavailable)?;
            if paired != owner {
                return Err(Error::Unavailable);
            }
            let journal = Arc::new(Journal::open(&journal_dir.path, worker).map_err(|error| {
                tracing::error!(
                    stage = "journal_open",
                    "BoxLite persistence home check failed"
                );
                error
            })?);
            if !defer_census {
                super::native::reconcile_home(&home.path, &journal).map_err(|_| {
                    tracing::error!(
                        stage = "native_reconciliation",
                        "BoxLite persistence home check failed"
                    );
                    Error::Unavailable
                })?;
            }
            (owner, journal)
        } else {
            // There is no adoption-by-empty-journal path. A new pair starts
            // with an empty dedicated home and an unused state directory.
            if fs::read_dir(&home.path)
                .map_err(|_| Error::Unavailable)?
                .any(|entry| entry.map_or(true, |entry| entry.file_name() != LOCK))
                || fs::read_dir(&journal_dir.path)
                    .map_err(|_| Error::Unavailable)?
                    .next()
                    .is_some()
            {
                return Err(Error::Unavailable);
            }
            let journal = Arc::new(Journal::open(&journal_dir.path, worker)?);
            if read_private(&journal_dir.path.join(PAIR), 8192)?.is_some()
                || !journal.snapshot()?.operations.is_empty()
                || !journal.snapshot()?.probes.is_empty()
            {
                return Err(Error::Unavailable);
            }
            let owner = Owner {
                version: 1,
                runtime_version: RUNTIME_VERSION.into(),
                worker_id: worker.into(),
                host_id: process.host_id,
                boot_id: process.boot_id,
                process_namespace: process.process_namespace,
                pair_id: uuid::Uuid::new_v4().to_string(),
                home,
                journal: journal_dir,
            };
            let bytes = serde_json::to_vec(&owner).map_err(|_| Error::Unavailable)?;
            if bytes.len() > 8192 {
                return Err(Error::Unavailable);
            }
            // Partial/uncertain pairing stays unavailable on the next start;
            // no SDK has been opened and no unique writable data is removed.
            write_atomic(&owner.journal.path, PAIR, &bytes)?;
            write_atomic(&owner.home.path, OWNER, &bytes)?;
            (owner, journal)
        };
        Ok(Self {
            owner,
            journal,
            _lock: lock,
        })
    }

    /// Call only before SDK initialization or after all SDK work has stopped.
    pub fn census(&self) -> Result<()> {
        self.verify(&self.owner.home.path, &self.owner.worker_id)?;
        let process = HostProcess::capture(std::process::id())?;
        if process.host_id != self.owner.host_id
            || process.boot_id != self.owner.boot_id
            || process.process_namespace != self.owner.process_namespace
        {
            return Err(Error::Unavailable);
        }
        super::native::preflight_home(&self.owner.home.path, &self.journal.snapshot()?)
            .map_err(|_| Error::Unavailable)
    }

    pub fn journal(&self) -> Arc<Journal> {
        self.journal.clone()
    }

    /// Ownership-only check for subsequent SDK calls. Full census is performed
    /// once before initialization, never against a concurrently mutating SDK.
    pub fn verify(&self, home: &Path, worker: &str) -> Result<()> {
        self.journal.verify_open(worker)?;
        if worker != self.owner.worker_id
            || Directory::inspect(home)? != self.owner.home
            || Directory::inspect(&self.owner.journal.path)? != self.owner.journal
        {
            return Err(Error::Unavailable);
        }
        for (root, file) in [
            (&self.owner.home.path, OWNER),
            (&self.owner.journal.path, PAIR),
        ] {
            let saved: Owner = serde_json::from_slice(
                &read_private(&root.join(file), 8192)?.ok_or(Error::Unavailable)?,
            )
            .map_err(|_| Error::Unavailable)?;
            if saved != self.owner {
                return Err(Error::Unavailable);
            }
        }
        Ok(())
    }
}
impl Drop for RuntimeHomeGuard {
    fn drop(&mut self) {
        let _ = self.journal.close();
        // HomeLock also unlocks explicitly on early construction errors.
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{symlink, PermissionsExt};
    struct Root(PathBuf);
    impl Root {
        fn new() -> Self {
            let root =
                std::env::temp_dir().join(format!("onlyboxes-home-owner-{}", uuid::Uuid::new_v4()));
            private_directory(&root).unwrap();
            Self(root)
        }
        fn home(&self) -> PathBuf {
            self.0.join("sdk")
        }
        fn state(&self) -> PathBuf {
            self.0.join("state")
        }
        fn open(&self) -> Result<RuntimeHomeGuard> {
            RuntimeHomeGuard::open(self.home(), self.state(), "worker")
        }
    }
    impl Drop for Root {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }
    #[test]
    fn paired_owner_reopens_but_rejects_other_workers_or_replaced_directories() {
        let root = Root::new();
        let owner = root.open().unwrap();
        assert!(root.open().is_err());
        assert!(owner.verify(&root.home(), "other-worker").is_err());
        drop(owner);
        assert!(RuntimeHomeGuard::open(root.home(), root.state(), "other-worker").is_err());
        assert!(root.open().is_ok());
        fs::rename(root.state(), root.0.join("old-state")).unwrap();
        private_directory(&root.state()).unwrap();
        assert!(root.open().is_err());
        assert!(!root.state().join("journal.json").exists());
    }
    #[test]
    fn copied_pair_and_foreign_host_or_namespace_are_not_adopted() {
        let root = Root::new();
        drop(root.open().unwrap());
        let copy = Root::new();
        private_directory(&copy.home()).unwrap();
        private_directory(&copy.state()).unwrap();
        fs::copy(root.home().join(OWNER), copy.home().join(OWNER)).unwrap();
        fs::copy(root.state().join(PAIR), copy.state().join(PAIR)).unwrap();
        assert!(copy.open().is_err());
        assert!(!copy.state().join("journal.json").exists());
        let bytes = read_private(&root.home().join(OWNER), 8192)
            .unwrap()
            .unwrap();
        for field in ["host_id", "boot_id", "process_namespace"] {
            let mut owner: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
            owner[field] = if field == "process_namespace" {
                "linux-pid:1:2".into()
            } else {
                uuid::Uuid::new_v4().to_string().into()
            };
            let changed = serde_json::to_vec(&owner).unwrap();
            write_atomic(&root.home(), OWNER, &changed).unwrap();
            write_atomic(&root.state(), PAIR, &changed).unwrap();
            assert!(root.open().is_err());
        }
        write_atomic(&root.home(), OWNER, &bytes).unwrap();
        write_atomic(&root.state(), PAIR, &bytes).unwrap();
        assert!(root.open().is_ok());
    }
    #[test]
    fn lost_history_or_owner_never_initializes_a_replacement_journal() {
        let root = Root::new();
        drop(root.open().unwrap());
        let journal = fs::read(root.state().join("journal.json")).unwrap();
        fs::remove_file(root.state().join("journal.json")).unwrap();
        assert!(root.open().is_err());
        assert!(!root.state().join("journal.json").exists());
        write_atomic(&root.state(), "journal.json", &journal).unwrap();
        fs::remove_file(root.home().join(OWNER)).unwrap();
        assert!(root.open().is_err());
        assert!(!root.home().join(OWNER).exists());
    }
    #[test]
    fn unmarked_native_data_and_orphan_directories_are_preserved_without_sdk_startup() {
        let root = Root::new();
        private_directory(&root.home()).unwrap();
        fs::write(root.home().join("unique-disk"), b"preserve").unwrap();
        assert!(root.open().is_err());
        assert!(!root.state().join("journal.json").exists());
        assert_eq!(
            fs::read(root.home().join("unique-disk")).unwrap(),
            b"preserve"
        );
        fs::remove_file(root.home().join("unique-disk")).unwrap();
        drop(root.open().unwrap());
        let orphan = root.home().join("boxes/orphan");
        private_directory(&orphan).unwrap();
        fs::write(orphan.join("disk.qcow2"), b"unique").unwrap();
        assert!(root.open().is_err());
        assert_eq!(fs::read(orphan.join("disk.qcow2")).unwrap(), b"unique");
    }
    #[test]
    fn redirected_or_public_markers_and_nested_roots_are_rejected() {
        let root = Root::new();
        drop(root.open().unwrap());
        let path = root.home().join(OWNER);
        fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).unwrap();
        assert!(root.open().is_err());
        fs::rename(&path, root.0.join("owner-backup")).unwrap();
        symlink(root.0.join("owner-backup"), &path).unwrap();
        assert!(root.open().is_err());
        let nested = Root::new();
        assert!(
            RuntimeHomeGuard::open(nested.home(), nested.home().join("journal"), "worker").is_err()
        );
    }
    #[test]
    fn failed_home_construction_unlocks_an_inherited_description() {
        let root = Root::new();
        drop(root.open().unwrap());
        fn rejected(root: &Root, alias: &mut Option<File>) -> Result<()> {
            let file = private_options()
                .read(true)
                .write(true)
                .open(root.home().join(LOCK))
                .map_err(|_| Error::Unavailable)?;
            file.try_lock().map_err(|_| Error::Unavailable)?;
            let held = HomeLock(file);
            *alias = Some(held.0.try_clone().map_err(|_| Error::Unavailable)?);
            // Mirrors an identity/census rejection after lock acquisition.
            Err(Error::Unavailable)
        }
        let mut inherited = None;
        assert!(rejected(&root, &mut inherited).is_err());
        let replacement = root.open().unwrap();
        assert!(inherited.as_ref().unwrap().try_lock().is_err());
        drop(inherited);
        assert!(root.open().is_err());
        drop(replacement);
        assert!(root.open().is_ok());
    }
    #[test]
    fn explicit_home_unlock_is_not_delayed_by_an_inherited_description() {
        let root = Root::new();
        let guard = root.open().unwrap();
        let alias = guard._lock.0.try_clone().unwrap();
        drop(guard);
        let replacement = root.open().unwrap();
        assert!(alias.try_lock().is_err());
        drop(alias);
        assert!(root.open().is_err());
        drop(replacement);
        assert!(root.open().is_ok());
    }
}

#[path = "home_execution_store.rs"]
mod execution_store;
