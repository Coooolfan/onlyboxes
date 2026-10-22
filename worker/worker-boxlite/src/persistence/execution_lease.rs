//! Durable receipts and a disk-independent expiry view for the lease enforcer.
//! The caller runs enforcement independently of the Worker and control I/O.
use super::execution_boot::{BootRecord, BootWatch};
use super::journal::{check_file, private_directory, private_options, read_private, write_atomic};
use super::types::{valid_id, valid_scope};
use super::{Error, HostProcess};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File};
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

pub(super) type Result<T> = std::result::Result<T, Error>;
pub(super) const MAX_RECORDS: usize = 16384;
pub(super) const MAX_BYTES: usize = 64 << 20;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExecutionLease {
    pub box_id: String,
    pub scoped_session_id: String,
    pub generation: i64,
    pub revision: u64,
    pub expires_unix_ms: i64,
    pub process: HostProcess,
    pub revoked: bool,
}
impl ExecutionLease {
    pub(super) fn validate(&self) -> Result<()> {
        if !valid_id(&self.box_id)
            || !valid_scope(&self.scoped_session_id)
            || self.generation <= 0
            || self.revision == 0
            || self.revision >= 1 << 62
            || self.expires_unix_ms <= 0
        {
            return Err(Error::Conflict);
        }
        self.process.validate_native_id(&self.box_id)
    }
    fn same_owner(&self, other: &Self) -> bool {
        self.box_id == other.box_id
            && self.scoped_session_id == other.scoped_session_id
            && self.generation == other.generation
            && self.process == other.process
    }
}

#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Owner {
    version: u32,
    worker_id: String,
    host_id: String,
    store_id: String,
    root: PathBuf,
    device: u64,
    inode: u64,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct State {
    version: u32,
    store_id: String,
    pub(super) leases: BTreeMap<String, ExecutionLease>,
    #[serde(default)]
    pub(super) boots: BTreeMap<String, BootRecord>,
}

#[derive(Clone, Copy)]
pub(super) struct Moment {
    wall: i64,
    monotonic: Instant,
}
impl Moment {
    pub(super) fn now() -> Result<Self> {
        Ok(Self {
            wall: i64::try_from(
                SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .map_err(|_| Error::Unavailable)?
                    .as_millis(),
            )
            .map_err(|_| Error::Unavailable)?,
            monotonic: Instant::now(),
        })
    }
}
#[derive(Clone)]
pub(super) struct Watch {
    process: HostProcess,
    expires: i64,
    deadline: Instant,
    pub(super) revoked: bool,
    pub(super) retired: bool,
}
impl Watch {
    pub(super) fn initial(lease: &ExecutionLease, now: Moment, revoked: bool) -> Result<Self> {
        let remaining = lease.expires_unix_ms.saturating_sub(now.wall).max(0) as u64;
        Ok(Self {
            process: lease.process.clone(),
            expires: lease.expires_unix_ms,
            deadline: now
                .monotonic
                .checked_add(Duration::from_millis(remaining))
                .ok_or(Error::Unavailable)?,
            revoked: revoked || lease.revoked || remaining == 0,
            retired: false,
        })
    }
    fn renewed(&self, lease: &ExecutionLease) -> Result<Self> {
        let delta = lease
            .expires_unix_ms
            .checked_sub(self.expires)
            .filter(|delta| *delta >= 0)
            .ok_or(Error::Conflict)?;
        Ok(Self {
            process: lease.process.clone(),
            expires: lease.expires_unix_ms,
            deadline: self
                .deadline
                .checked_add(Duration::from_millis(delta as u64))
                .ok_or(Error::Unavailable)?,
            revoked: false,
            retired: false,
        })
    }
    pub(super) fn live(&mut self, now: Moment) -> bool {
        if now.wall >= self.expires || now.monotonic >= self.deadline {
            self.revoked = true;
        }
        !self.revoked && !self.retired
    }
}

#[derive(Debug, Default, PartialEq, Eq)]
pub struct LeaseEnforcement {
    pub attempted: usize,
    pub failed: usize,
}

/// Dedicated private ledger, never the Worker's lifecycle journal. Restart
/// invalidates all historical live grants: monotonic continuity is unknown.
/// Native roots are not deleted and missing history is never initialized away.
pub struct ExecutionLeaseLedger {
    root: PathBuf,
    owner: Owner,
    lock: File,
    pub(super) state: Mutex<State>,
    pub(super) watches: RwLock<BTreeMap<String, Arc<Mutex<Watch>>>>,
    pub(super) poisoned: AtomicBool,
    pub(super) boot_watches: RwLock<BTreeMap<String, Arc<Mutex<BootWatch>>>>,
    cursor: AtomicUsize,
    pub(super) boot_cursor: AtomicUsize,
    control_claimed: AtomicBool,
    persist: fn(&Path, &State) -> Result<()>,
}
impl ExecutionLeaseLedger {
    pub fn open(root: impl AsRef<Path>, worker: &str) -> Result<Self> {
        Self::open_inner(root.as_ref(), worker, None)
    }

    /// Reopen a previously provisioned store using the identity pinned by the
    /// Worker journal. Loss of the whole directory cannot create an empty store.
    pub fn open_bound(root: impl AsRef<Path>, worker: &str, store_id: &str) -> Result<Self> {
        let id = uuid::Uuid::parse_str(store_id).map_err(|_| Error::Unavailable)?;
        if id.is_nil() || id.to_string() != store_id {
            return Err(Error::Unavailable);
        }
        Self::open_inner(root.as_ref(), worker, Some(store_id))
    }

    fn open_inner(root: &Path, worker: &str, expected: Option<&str>) -> Result<Self> {
        if !root.is_absolute()
            || worker.trim().is_empty()
            || worker.trim() != worker
            || worker.len() > 256
            || worker.chars().any(char::is_control)
        {
            return Err(Error::Unavailable);
        }
        if expected.is_some() {
            fs::symlink_metadata(root).map_err(|_| Error::Unavailable)?;
        }
        private_directory(root)?;
        let root = fs::canonicalize(root).map_err(|_| Error::Unavailable)?;
        let directory = fs::metadata(&root).map_err(|_| Error::Unavailable)?;
        let lock = private_options()
            .read(true)
            .write(true)
            .create(true)
            .open(root.join("guard.lock"))
            .map_err(|_| Error::Unavailable)?;
        check_file(&lock, 0)?;
        lock.try_lock().map_err(|_| Error::Unavailable)?;
        let current = HostProcess::capture(std::process::id())?;
        let saved = read_private(&root.join("owner.json"), 64 << 10)?;
        let (owner, state) = if let Some(bytes) = saved {
            let owner: Owner = serde_json::from_slice(&bytes).map_err(|_| Error::Unavailable)?;
            let id = uuid::Uuid::parse_str(&owner.store_id).map_err(|_| Error::Unavailable)?;
            if owner.version != 1
                || owner.worker_id != worker
                || owner.host_id != current.host_id
                || id.is_nil()
                || id.to_string() != owner.store_id
                || owner.root != root
                || owner.device != directory.dev()
                || owner.inode != directory.ino()
                || expected.is_some_and(|expected| owner.store_id != expected)
            {
                return Err(Error::Unavailable);
            }
            let state: State = serde_json::from_slice(
                &read_private(&root.join("leases.json"), MAX_BYTES)?.ok_or(Error::Unavailable)?,
            )
            .map_err(|_| Error::Unavailable)?;
            if state.version != 1 || state.store_id != owner.store_id {
                return Err(Error::Unavailable);
            }
            (owner, state)
        } else {
            if expected.is_some() {
                return Err(Error::Unavailable);
            }
            if fs::read_dir(&root)
                .map_err(|_| Error::Unavailable)?
                .any(|entry| entry.map_or(true, |entry| entry.file_name() != "guard.lock"))
            {
                return Err(Error::Unavailable);
            }
            let owner = Owner {
                version: 1,
                worker_id: worker.into(),
                host_id: current.host_id.clone(),
                store_id: uuid::Uuid::new_v4().to_string(),
                root: root.clone(),
                device: directory.dev(),
                inode: directory.ino(),
            };
            let state = State {
                version: 1,
                store_id: owner.store_id.clone(),
                leases: BTreeMap::new(),
                boots: BTreeMap::new(),
            };
            persist_state(&root, &state)?;
            write_atomic(
                &root,
                "owner.json",
                &serde_json::to_vec(&owner).map_err(|_| Error::Unavailable)?,
            )?;
            (owner, state)
        };
        if state.leases.len() > MAX_RECORDS || state.boots.len() > MAX_RECORDS {
            return Err(Error::Unavailable);
        }
        let now = Moment::now()?;
        let mut scopes = BTreeSet::new();
        let mut watches = BTreeMap::new();
        for (id, lease) in &state.leases {
            lease.validate().map_err(|_| Error::Unavailable)?;
            if id != &lease.box_id
                || !scopes.insert(&lease.scoped_session_id)
                || lease.process.host_id != owner.host_id
                || (lease.process.boot_id == current.boot_id
                    && lease.process.process_namespace != current.process_namespace)
            {
                return Err(Error::Unavailable);
            }
            watches.insert(
                id.clone(),
                Arc::new(Mutex::new(Watch::initial(lease, now, true)?)),
            );
        }
        let mut boot_watches = BTreeMap::new();
        let mut boot_scopes = BTreeSet::new();
        let mut boot_operations = BTreeSet::new();
        for (id, record) in &state.boots {
            let grant = &record.authorization;
            grant.validate().map_err(|_| Error::Unavailable)?;
            if id != &grant.box_id
                || !boot_scopes.insert(&grant.scoped_session_id)
                || !boot_operations.insert(&grant.operation_id)
                || grant.origin.host_id != owner.host_id
                || (grant.origin.boot_id == current.boot_id
                    && grant.origin.process_namespace != current.process_namespace)
                || state.leases.values().any(|lease| {
                    lease.box_id != *id && lease.scoped_session_id == grant.scoped_session_id
                })
            {
                return Err(Error::Unavailable);
            }
            if let Some(process) = &record.bound_process {
                process
                    .validate_native_id(id)
                    .map_err(|_| Error::Unavailable)?;
                let lease = state.leases.get(id).ok_or(Error::Unavailable)?;
                if lease.process != *process
                    || lease.generation != grant.generation
                    || lease.scoped_session_id != grant.scoped_session_id
                    || lease.revision <= grant.prior_revision.unwrap_or(0)
                    || lease.expires_unix_ms < grant.expires_unix_ms
                {
                    return Err(Error::Unavailable);
                }
            } else if let Some(lease) = state.leases.get(id) {
                if Some(lease.revision) != grant.prior_revision
                    || lease.generation != grant.generation
                    || lease.scoped_session_id != grant.scoped_session_id
                {
                    return Err(Error::Unavailable);
                }
            } else if grant.prior_revision.is_some() {
                return Err(Error::Unavailable);
            }
            if record.bound_process.is_none() {
                if let Some(old) = watches.get(id) {
                    old.lock().map_err(|_| Error::Unavailable)?.retired = true;
                }
            }
            boot_watches.insert(
                id.clone(),
                Arc::new(Mutex::new(BootWatch::new(record, now, true)?)),
            );
        }
        // Recognized interrupted atomic writes can be removed only after the
        // complete owner/state pair has passed validation. Unknown files stay.
        for entry in fs::read_dir(&root).map_err(|_| Error::Unavailable)? {
            let entry = entry.map_err(|_| Error::Unavailable)?;
            let name = entry
                .file_name()
                .into_string()
                .map_err(|_| Error::Unavailable)?;
            if matches!(name.as_str(), "owner.json" | "leases.json" | "guard.lock") {
                continue;
            }
            let temp = name
                .strip_prefix("journal-")
                .and_then(|s| s.strip_suffix(".tmp"))
                .and_then(|s| uuid::Uuid::parse_str(s).ok());
            if temp.is_none() || read_private(&entry.path(), MAX_BYTES)?.is_none() {
                return Err(Error::Unavailable);
            }
            fs::remove_file(entry.path()).map_err(|_| Error::Unavailable)?;
        }
        Ok(Self {
            root,
            owner,
            lock,
            state: Mutex::new(state),
            watches: RwLock::new(watches),
            poisoned: AtomicBool::new(false),
            boot_watches: RwLock::new(boot_watches),
            cursor: AtomicUsize::new(0),
            boot_cursor: AtomicUsize::new(0),
            control_claimed: AtomicBool::new(false),
            persist: persist_state,
        })
    }

    pub fn store_id(&self) -> &str {
        &self.owner.store_id
    }

    pub(super) fn worker_id(&self) -> &str {
        &self.owner.worker_id
    }
    pub(super) fn claim_control(&self) -> Result<()> {
        self.control_claimed
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
            .map(|_| ())
            .map_err(|_| Error::Unavailable)
    }
    pub(super) fn separate_control_dir(&self, directory: &Path) -> bool {
        !directory.starts_with(&self.root) && !self.root.starts_with(directory)
    }
    pub(super) fn control_available(&self) -> Result<()> {
        self.available()
    }
    pub(super) fn close_admission(&self) {
        self.poisoned.store(true, Ordering::SeqCst);
    }
    pub(super) fn writers_stopped(&self) -> Result<bool> {
        if !self.boots_settled()? {
            return Ok(false);
        }
        let slots: Vec<_> = self
            .watches
            .read()
            .map_err(|_| Error::Unavailable)?
            .values()
            .cloned()
            .collect();
        for slot in slots {
            if !slot
                .lock()
                .map_err(|_| Error::Unavailable)?
                .process
                .writers_exited()?
            {
                return Ok(false);
            }
        }
        Ok(true)
    }
    #[cfg(test)]
    pub(super) fn hold_mutations_for_test(
        &self,
        ready: std::sync::mpsc::Sender<()>,
        release: std::sync::mpsc::Receiver<()>,
    ) {
        let _state = self.state.lock().unwrap();
        ready.send(()).unwrap();
        let _ = release.recv_timeout(Duration::from_secs(5));
    }

    pub fn status(&self, id: &str) -> Result<Option<ExecutionLease>> {
        self.available()?;
        let state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        let Some(lease) = state.leases.get(id) else {
            return Ok(None);
        };
        let mut lease = lease.clone();
        lease.revoked |= !self
            .watch(id)?
            .lock()
            .map_err(|_| Error::Unavailable)?
            .live(Moment::now()?);
        Ok(Some(lease))
    }

    /// Read-only execution authority, including the exact live kernel writer.
    /// Unlike status, stopped predecessors are rejected rather than returned.
    pub fn check_running(&self, id: &str) -> Result<ExecutionLease> {
        let lease = self.status(id)?.ok_or(Error::Conflict)?;
        if lease.revoked {
            return Err(Error::Conflict);
        }
        lease.process.verify_current_writer(id)?;
        Ok(lease)
    }

    /// Initial adoption of an already-running, exactly pinned process. This
    /// cannot authorize a controlled boot or rearm a revoked/expired record.
    pub fn adopt(&self, lease: ExecutionLease) -> Result<ExecutionLease> {
        lease.validate()?;
        self.available()?;
        if lease.generation != 1 || lease.revision != 1 || lease.revoked {
            return Err(Error::Conflict);
        }
        let mut candidate = Watch::initial(&lease, Moment::now()?, false)?;
        if !candidate.live(Moment::now()?) {
            return Err(Error::Conflict);
        }
        lease.process.verify_current_writer(&lease.box_id)?;
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        if state.boots.values().any(|record| {
            record.authorization.box_id == lease.box_id
                || record.authorization.scoped_session_id == lease.scoped_session_id
        }) {
            return Err(Error::Conflict);
        }
        if let Some(prior) = state.leases.get(&lease.box_id) {
            return if prior == &lease
                && self
                    .watch(&lease.box_id)?
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .live(Moment::now()?)
            {
                Ok(prior.clone())
            } else {
                Err(Error::Conflict)
            };
        }
        if state.leases.len() >= MAX_RECORDS
            || state
                .leases
                .values()
                .any(|old| old.scoped_session_id == lease.scoped_session_id)
        {
            return Err(Error::Conflict);
        }
        let mut next = state.clone();
        next.leases.insert(lease.box_id.clone(), lease.clone());
        // Publish a stop target before the first uncertain disk mutation; the
        // enforcement view never waits for this state mutex or fsync.
        let slot = Arc::new(Mutex::new(candidate));
        self.watches
            .write()
            .map_err(|_| Error::Unavailable)?
            .insert(lease.box_id.clone(), slot.clone());
        self.commit(&mut state, next)?;
        let mut watch = slot.lock().map_err(|_| Error::Unavailable)?;
        if !watch.live(Moment::now()?)
            || lease.process.verify_current_writer(&lease.box_id).is_err()
        {
            watch.revoked = true;
            return Err(Error::Unavailable);
        }
        Ok(lease)
    }

    pub fn renew(&self, expected_revision: u64, lease: ExecutionLease) -> Result<ExecutionLease> {
        lease.validate()?;
        self.available()?;
        if lease.revoked || expected_revision.checked_add(1) != Some(lease.revision) {
            return Err(Error::Conflict);
        }
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        let prior = state.leases.get(&lease.box_id).ok_or(Error::Conflict)?;
        let old = self.watch(&lease.box_id)?;
        if !old
            .lock()
            .map_err(|_| Error::Unavailable)?
            .live(Moment::now()?)
            || prior.revoked
        {
            return Err(Error::Conflict);
        }
        if prior == &lease {
            lease.process.verify_current_writer(&lease.box_id)?;
            return Ok(lease);
        }
        if prior.revision != expected_revision
            || !prior.same_owner(&lease)
            || lease.expires_unix_ms < prior.expires_unix_ms
        {
            return Err(Error::Conflict);
        }
        lease.process.verify_current_writer(&lease.box_id)?;
        let mut candidate = old
            .lock()
            .map_err(|_| Error::Unavailable)?
            .renewed(&lease)?;
        let mut next = state.clone();
        next.leases.insert(lease.box_id.clone(), lease.clone());
        self.commit(&mut state, next)?;
        // A stalled disk write cannot extend the old enforcement deadline.
        let mut watch = old.lock().map_err(|_| Error::Unavailable)?;
        if !watch.live(Moment::now()?)
            || !candidate.live(Moment::now()?)
            || lease.process.verify_current_writer(&lease.box_id).is_err()
        {
            watch.revoked = true;
            return Err(Error::Unavailable);
        }
        *watch = candidate;
        Ok(lease)
    }

    /// Periodic native enforcement: no ledger reads, writes or mutation locks.
    /// A failed request remains watched; delivery is never proof of exit.
    pub fn enforce(&self) -> Result<LeaseEnforcement> {
        let watches: Vec<_> = self
            .watches
            .read()
            .map_err(|_| Error::Unavailable)?
            .values()
            .cloned()
            .collect();
        let now = Moment::now();
        let poisoned = self.poisoned.load(Ordering::SeqCst);
        let mut expired = Vec::new();
        for slot in watches {
            let mut watch = slot.lock().map_err(|_| Error::Unavailable)?;
            if watch.retired {
                continue;
            }
            let expired_now = match now {
                Ok(now) => !watch.live(now),
                Err(_) => true,
            };
            if poisoned || expired_now {
                watch.revoked = true;
                drop(watch);
                expired.push(slot);
            }
        }
        let mut result = self.enforce_known_boots()?;
        if !expired.is_empty() {
            let start = self.cursor.fetch_add(64, Ordering::SeqCst) % expired.len();
            for offset in 0..expired.len().min(64) {
                let mut watch = expired[(start + offset) % expired.len()]
                    .lock()
                    .map_err(|_| Error::Unavailable)?;
                if watch.retired {
                    continue;
                }
                // Serialize native delivery with lease publication, without a
                // disk mutation lock. A stale scan cannot stop a renewed lease.
                watch.revoked = true;
                result.attempted += 1;
                if watch.process.request_writer_stop().is_err() {
                    result.failed += 1;
                }
            }
        }
        Ok(result)
    }

    pub(super) fn available(&self) -> Result<()> {
        if self.poisoned.load(Ordering::SeqCst) {
            return Err(Error::Unavailable);
        }
        if self.verify_binding().is_err() {
            self.poisoned.store(true, Ordering::SeqCst);
            return Err(Error::Unavailable);
        }
        Ok(())
    }
    fn verify_binding(&self) -> Result<()> {
        let directory = fs::symlink_metadata(&self.root).map_err(|_| Error::Unavailable)?;
        // SAFETY: geteuid only reads the caller's identity.
        let uid = unsafe { libc::geteuid() };
        if !directory.is_dir()
            || directory.dev() != self.owner.device
            || directory.ino() != self.owner.inode
            || directory.mode() & 0o077 != 0
            || directory.uid() != uid
        {
            return Err(Error::Unavailable);
        }
        check_file(&self.lock, 0)?;
        let locked = self.lock.metadata().map_err(|_| Error::Unavailable)?;
        let current =
            fs::symlink_metadata(self.root.join("guard.lock")).map_err(|_| Error::Unavailable)?;
        if !current.is_file() || current.dev() != locked.dev() || current.ino() != locked.ino() {
            return Err(Error::Unavailable);
        }
        let owner: Owner = serde_json::from_slice(
            &read_private(&self.root.join("owner.json"), 64 << 10)?.ok_or(Error::Unavailable)?,
        )
        .map_err(|_| Error::Unavailable)?;
        if owner != self.owner {
            return Err(Error::Unavailable);
        }
        Ok(())
    }
    pub(super) fn watch(&self, id: &str) -> Result<Arc<Mutex<Watch>>> {
        self.watches
            .read()
            .map_err(|_| Error::Unavailable)?
            .get(id)
            .cloned()
            .ok_or(Error::Unavailable)
    }
    pub(super) fn commit(&self, state: &mut State, next: State) -> Result<()> {
        self.available()?;
        if (self.persist)(&self.root, &next).is_err() {
            self.poisoned.store(true, Ordering::SeqCst);
            return Err(Error::Unavailable);
        }
        self.available()?;
        *state = next;
        Ok(())
    }
}
impl Drop for ExecutionLeaseLedger {
    fn drop(&mut self) {
        let _ = self.lock.unlock();
    }
}
fn persist_state(root: &Path, state: &State) -> Result<()> {
    let bytes = serde_json::to_vec(state).map_err(|_| Error::Unavailable)?;
    if bytes.len() > MAX_BYTES {
        return Err(Error::Unavailable);
    }
    write_atomic(root, "leases.json", &bytes)
}

#[cfg(test)]
#[path = "execution_lease_tests.rs"]
mod tests;
