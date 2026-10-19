use std::collections::BTreeMap;
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

use serde::{Deserialize, Serialize};

use super::types::{valid_id, valid_scope};
use super::{artifact_name, Error, Kind, Operation, Phase};

type Result<T> = std::result::Result<T, Error>;
const MAX_JOURNAL_BYTES: usize = 64 << 20;

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct JournalState {
    pub version: u32,
    pub worker_id: String,
    pub current: BTreeMap<String, String>,
    pub operations: BTreeMap<String, Operation>,
    pub probes: BTreeMap<String, String>,
}

impl JournalState {
    fn validate(&self, worker: &str) -> Result<()> {
        if self.version != 1 || self.worker_id != worker {
            return Err(Error::Unavailable);
        }
        for (id, op) in &self.operations {
            if id != &op.operation_id
                || op.validate().is_err()
                || !self.current.contains_key(&op.scoped_session_id)
            {
                return Err(Error::Unavailable);
            }
            let current = self
                .operations
                .get(&self.current[&op.scoped_session_id])
                .ok_or(Error::Unavailable)?;
            if (op.kind != Kind::Cancel && op.generation > current.generation)
                || (op.kind == Kind::Delete && current.operation_id != op.operation_id)
            {
                return Err(Error::Unavailable);
            }
        }
        for (scope, id) in &self.current {
            let Some(op) = self.operations.get(id) else {
                return Err(Error::Unavailable);
            };
            if scope != &op.scoped_session_id {
                return Err(Error::Unavailable);
            }
        }
        for (scope, id) in &self.probes {
            if !valid_scope(scope)
                || !valid_id(id)
                || self
                    .current
                    .get(scope)
                    .is_some_and(|op| self.operations[op].generation > 1)
            {
                return Err(Error::Unavailable);
            }
        }
        Ok(())
    }
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Owner {
    version: u32,
    worker_id: String,
}

struct Inner {
    state: JournalState,
    lock: Option<File>,
    poisoned: bool,
}

/// Process-exclusive private journal. All mutations are copied, validated,
/// synced and atomically renamed before acknowledgement. A failed commit,
/// including a possibly successful rename, poisons this handle until reopen.
pub struct Journal {
    root: PathBuf,
    inner: Mutex<Inner>,
    persist: fn(&Path, &JournalState) -> Result<()>,
}

impl Journal {
    pub fn open(root: impl AsRef<Path>, worker: &str) -> Result<Self> {
        let root = root.as_ref();
        if !root.is_absolute()
            || worker.trim().is_empty()
            || worker.trim() != worker
            || worker.len() > 256
            || worker.chars().any(char::is_control)
        {
            return Err(Error::Unavailable);
        }
        private_directory(root)?;
        let root = fs::canonicalize(root).map_err(|_| Error::Unavailable)?;
        let lock = private_options()
            .read(true)
            .write(true)
            .create(true)
            .open(root.join("journal.lock"))
            .map_err(|_| Error::Unavailable)?;
        check_file(&lock, 0)?;
        lock.try_lock().map_err(|_| Error::Unavailable)?;
        let owner = read_private(&root.join("owner.json"), 1024)?;
        if let Some(bytes) = &owner {
            let owner: Owner = serde_json::from_slice(bytes).map_err(|_| Error::Unavailable)?;
            if owner.version != 1 || owner.worker_id != worker {
                return Err(Error::Unavailable);
            }
        }
        let bytes = read_private(&root.join("journal.json"), MAX_JOURNAL_BYTES)?;
        let state = if let Some(bytes) = bytes {
            let state: JournalState =
                serde_json::from_slice(&bytes).map_err(|_| Error::Unavailable)?;
            state.validate(worker)?;
            state
        } else {
            // A separate durable owner marker distinguishes missing history
            // from an unused directory. Never reset a lost journal to gen 1.
            if owner.is_some()
                || fs::read_dir(&root)
                    .map_err(|_| Error::Unavailable)?
                    .any(|entry| entry.map_or(true, |entry| entry.file_name() != "journal.lock"))
            {
                return Err(Error::Unavailable);
            }
            let state = JournalState {
                version: 1,
                worker_id: worker.to_owned(),
                current: BTreeMap::new(),
                operations: BTreeMap::new(),
                probes: BTreeMap::new(),
            };
            persist_state(&root, &state)?;
            state
        };
        if owner.is_none() {
            let owner = serde_json::to_vec(&Owner {
                version: 1,
                worker_id: worker.to_owned(),
            })
            .map_err(|_| Error::Unavailable)?;
            write_atomic(&root, "owner.json", &owner)?;
        }
        private_directory(&root.join("artifacts"))?;
        Ok(Self {
            root,
            inner: Mutex::new(Inner {
                state,
                lock: Some(lock),
                poisoned: false,
            }),
            persist: persist_state,
        })
    }

    fn inner(&self) -> Result<MutexGuard<'_, Inner>> {
        let inner = self.inner.lock().map_err(|_| Error::Unavailable)?;
        if inner.poisoned || inner.lock.is_none() {
            return Err(Error::Unavailable);
        }
        Ok(inner)
    }

    pub fn close(&self) -> Result<()> {
        let mut inner = self.inner.lock().map_err(|_| Error::Unavailable)?;
        inner.lock.take();
        Ok(())
    }

    pub fn snapshot(&self) -> Result<JournalState> {
        Ok(self.inner()?.state.clone())
    }

    fn transaction(&self, change: impl FnOnce(&mut JournalState) -> Result<()>) -> Result<()> {
        let mut inner = self.inner()?;
        let mut next = inner.state.clone();
        change(&mut next)?;
        next.validate(&inner.state.worker_id)?;
        if (self.persist)(&self.root, &next).is_err() {
            inner.poisoned = true;
            return Err(Error::Unavailable);
        }
        inner.state = next;
        Ok(())
    }

    pub fn claim(&self, op: Operation) -> Result<()> {
        op.validate()?;
        if !op.initial() {
            return Err(Error::Conflict);
        }
        self.transaction(|state| {
            if op.generation == 1
                && matches!(op.kind, Kind::Source | Kind::Adopt)
                && state
                    .probes
                    .get(&op.scoped_session_id)
                    .is_some_and(|id| id != &op.box_id)
            {
                return Err(Error::Conflict);
            }
            if let Some(prior) = state.operations.get(&op.operation_id) {
                if state.current.get(&op.scoped_session_id) != Some(&op.operation_id)
                    || prior.scoped_session_id != op.scoped_session_id
                    || prior.generation != op.generation
                    || prior.kind != op.kind
                    || prior.restore_spec != op.restore_spec
                    || prior.staging_bytes != op.staging_bytes
                    || prior.lease_ttl_sec != op.lease_ttl_sec
                    || (op.source_process.is_some() && prior.source_process != op.source_process)
                    || (op.source_stop_confirmed && !prior.source_stop_confirmed)
                    || (op.kind == Kind::Target && prior.archive != op.archive)
                    || (!op.box_id.is_empty() && prior.box_id != op.box_id)
                    || (!op.tombstone_box_id.is_empty()
                        && prior.tombstone_box_id != op.tombstone_box_id)
                {
                    return Err(Error::Conflict);
                }
                return Ok(());
            }
            if let Some(id) = state.current.get(&op.scoped_session_id) {
                let prior = &state.operations[id];
                if prior.kind == Kind::Delete
                    || (prior.kind != Kind::Cancel && prior.restore_spec != op.restore_spec)
                {
                    return Err(Error::Conflict);
                }
                let may_replace = (op.kind == Kind::Delete && prior.generation < op.generation)
                    || (prior.active()
                        && op.kind == Kind::Source
                        && prior.generation == op.generation
                        && prior.box_id == op.box_id)
                    || (prior.terminal()
                        && op.kind == Kind::Target
                        && prior.generation < op.generation);
                if !may_replace {
                    return Err(Error::Conflict);
                }
            }
            if op.generation > 1 {
                state.probes.remove(&op.scoped_session_id);
            }
            state
                .current
                .insert(op.scoped_session_id.clone(), op.operation_id.clone());
            state.operations.insert(op.operation_id.clone(), op);
            Ok(())
        })
    }

    pub fn update(
        &self,
        id: &str,
        change: impl FnOnce(&mut Operation) -> Result<()>,
    ) -> Result<()> {
        self.transaction(|state| {
            let prior = state.operations.get(id).ok_or(Error::Conflict)?;
            if state
                .current
                .get(&prior.scoped_session_id)
                .map(String::as_str)
                != Some(id)
            {
                return Err(Error::Conflict);
            }
            let mut next = prior.clone();
            change(&mut next)?;
            next.validate()?;
            if !prior.can_update_to(&next) {
                return Err(Error::Conflict);
            }
            state.operations.insert(id.to_owned(), next);
            Ok(())
        })
    }

    pub fn cancel_unknown(&self, id: &str, scope: &str, generation: i64) -> Result<()> {
        if !valid_id(id) || !valid_scope(scope) || generation <= 0 {
            return Err(Error::Conflict);
        }
        self.transaction(|state| {
            if state.operations.contains_key(id) {
                return Err(Error::Conflict);
            }
            let current = state.current.get(scope).map(|id| &state.operations[id]);
            if current.is_some_and(|op| op.kind == Kind::Delete) {
                return Err(Error::Conflict);
            }
            if current.is_none_or(|op| op.terminal() && op.generation < generation) {
                state.current.insert(scope.to_owned(), id.to_owned());
            }
            let op = Operation {
                operation_id: id.to_owned(),
                scoped_session_id: scope.to_owned(),
                generation,
                kind: Kind::Cancel,
                phase: Phase::Canceled,
                restore_spec: None,
                archive: None,
                artifact: String::new(),
                box_id: String::new(),
                tombstone_box_id: String::new(),
                source_process: None,
                source_stop_confirmed: false,
                lease_expires_unix_ms: 0,
                staging_bytes: 0,
                snapshot_committed: false,
                verified: false,
                remote_verified: false,
                cache_until_unix_ms: 0,
                lease_ttl_sec: 0,
            };
            state.operations.insert(id.to_owned(), op);
            Ok(())
        })
    }

    pub fn probe(&self, scope: &str, box_id: &str) -> Result<()> {
        if !valid_scope(scope) || !valid_id(box_id) {
            return Err(Error::Conflict);
        }
        self.transaction(|state| {
            if state.current.contains_key(scope)
                || state.probes.get(scope).is_some_and(|id| id != box_id)
            {
                return Err(Error::Conflict);
            }
            state.probes.insert(scope.to_owned(), box_id.to_owned());
            Ok(())
        })
    }

    pub fn artifact_path(&self, scope: &str, operation: &str, generation: i64) -> Result<PathBuf> {
        if !valid_scope(scope) || !valid_id(operation) || generation <= 0 {
            return Err(Error::Conflict);
        }
        let _inner = self.inner()?;
        Ok(self
            .root
            .join("artifacts")
            .join(artifact_name(scope, operation, generation)))
    }
}

fn private_options() -> OpenOptions {
    let mut options = OpenOptions::new();
    options
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NONBLOCK);
    options
}

fn private_directory(path: &Path) -> Result<()> {
    create_directory(path)?;
    let meta = fs::symlink_metadata(path).map_err(|_| Error::Unavailable)?;
    // SAFETY: geteuid reads the calling process identity and has no pointers.
    if !meta.is_dir()
        || meta.permissions().mode() & 0o077 != 0
        || meta.uid() != unsafe { libc::geteuid() }
    {
        return Err(Error::Unavailable);
    }
    Ok(())
}

fn create_directory(path: &Path) -> Result<()> {
    let create =
        || std::os::unix::fs::DirBuilderExt::mode(&mut fs::DirBuilder::new(), 0o700).create(path);
    match create() {
        Ok(()) => {}
        Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => return Ok(()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            create_directory(path.parent().ok_or(Error::Unavailable)?)?;
            match create() {
                Ok(()) => {}
                Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => return Ok(()),
                Err(_) => return Err(Error::Unavailable),
            }
        }
        Err(_) => return Err(Error::Unavailable),
    }
    File::open(path.parent().ok_or(Error::Unavailable)?)
        .and_then(|parent| parent.sync_all())
        .map_err(|_| Error::Unavailable)
}

fn check_file(file: &File, limit: usize) -> Result<()> {
    let meta = file.metadata().map_err(|_| Error::Unavailable)?;
    // SAFETY: geteuid reads the calling process identity and has no pointers.
    if !meta.is_file()
        || meta.permissions().mode() & 0o077 != 0
        || meta.nlink() != 1
        || meta.uid() != unsafe { libc::geteuid() }
        || meta.len() > limit as u64
    {
        return Err(Error::Unavailable);
    }
    Ok(())
}

fn read_private(path: &Path, limit: usize) -> Result<Option<Vec<u8>>> {
    let file = match private_options().read(true).open(path) {
        Ok(file) => file,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err(Error::Unavailable),
    };
    check_file(&file, limit)?;
    let mut data = Vec::new();
    file.take(limit as u64 + 1)
        .read_to_end(&mut data)
        .map_err(|_| Error::Unavailable)?;
    if data.len() > limit {
        return Err(Error::Unavailable);
    }
    Ok(Some(data))
}

fn persist_state(root: &Path, state: &JournalState) -> Result<()> {
    let data = serde_json::to_vec(state).map_err(|_| Error::Unavailable)?;
    if data.len() > MAX_JOURNAL_BYTES {
        return Err(Error::Unavailable);
    }
    write_atomic(root, "journal.json", &data)
}

fn write_atomic(root: &Path, name: &str, data: &[u8]) -> Result<()> {
    let temp = root.join(format!("journal-{}.tmp", uuid::Uuid::new_v4()));
    let result = (|| {
        let mut file = private_options().write(true).create_new(true).open(&temp)?;
        file.write_all(data)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp, root.join(name))?;
        File::open(root)?.sync_all()
    })();
    let _ = fs::remove_file(temp);
    result.map_err(|_: std::io::Error| Error::Unavailable)
}

#[cfg(test)]
#[path = "journal_tests.rs"]
mod tests;
