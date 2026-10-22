//! Pre-boot grants retain unknown outcomes; only an exact live writer can bind
//! them into the ordinary renewable execution lease. Discovery is positive
//! evidence and runs separately from the known-writer expiry thread.
use super::execution_lease::*;
use super::types::{valid_id, valid_scope};
use super::{BootExecutable, Error, ExecutionLease, ExecutionLeaseLedger, HostProcess};
use serde::{Deserialize, Serialize};
use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BootAuthorization {
    pub operation_id: String,
    pub box_id: String,
    pub scoped_session_id: String,
    pub generation: i64,
    pub prior_revision: Option<u64>,
    pub expires_unix_ms: i64,
    pub origin: HostProcess,
    pub executable: BootExecutable,
}
impl BootAuthorization {
    pub(super) fn validate(&self) -> Result<()> {
        if !valid_id(&self.operation_id)
            || !valid_id(&self.box_id)
            || !valid_scope(&self.scoped_session_id)
            || self.generation <= 0
            || self.expires_unix_ms <= 0
            || self
                .prior_revision
                .is_some_and(|r| r == 0 || r >= (1 << 62) - 1)
        {
            return Err(Error::Conflict);
        }
        self.origin.validate()?;
        // The origin is a Worker process identity, not a guest writer identity.
        if self.origin.writer_cgroup.is_some() || self.origin.signal_version.is_some() {
            return Err(Error::Conflict);
        }
        self.executable.validate_metadata(&self.box_id)
    }
    fn lease(&self, process: HostProcess) -> ExecutionLease {
        ExecutionLease {
            box_id: self.box_id.clone(),
            scoped_session_id: self.scoped_session_id.clone(),
            generation: self.generation,
            revision: self.prior_revision.unwrap_or(0) + 1,
            expires_unix_ms: self.expires_unix_ms,
            process,
            revoked: false,
        }
    }
    fn origin_current(&self) -> Result<()> {
        if HostProcess::capture(self.origin.pid)? != self.origin {
            return Err(Error::Conflict);
        }
        Ok(())
    }
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct BootRecord {
    pub authorization: BootAuthorization,
    pub bound_process: Option<HostProcess>,
}

pub(super) struct BootWatch {
    authorization: BootAuthorization,
    clock: Watch,
    bound: bool,
    known: Vec<HostProcess>,
}
impl BootWatch {
    pub(super) fn new(record: &BootRecord, now: Moment, revoked: bool) -> Result<Self> {
        Ok(Self {
            authorization: record.authorization.clone(),
            // This clock never signals the origin; it is solely an expiry view.
            clock: Watch::initial(
                &record
                    .authorization
                    .lease(record.authorization.origin.clone()),
                now,
                revoked,
            )?,
            bound: record.bound_process.is_some(),
            known: Vec::new(),
        })
    }
}

impl ExecutionLeaseLedger {
    /// Must be acknowledged before issuing SDK start. The caller owns that
    /// native task through completion, then binds its exact resulting writer.
    /// A stopped predecessor may be rearmed only with its current CAS revision.
    pub fn prepare_boot(&self, authorization: BootAuthorization) -> Result<BootAuthorization> {
        authorization.validate()?;
        self.available()?;
        authorization.origin_current()?;
        authorization.executable.verify()?;
        let record = BootRecord {
            authorization: authorization.clone(),
            bound_process: None,
        };
        let mut candidate = BootWatch::new(&record, Moment::now()?, false)?;
        if !candidate.clock.live(Moment::now()?) {
            return Err(Error::Conflict);
        }
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        if state.retirements.contains_key(&authorization.box_id)
            || state.retirements.values().any(|status| {
                status.retirement.scoped_session_id == authorization.scoped_session_id
                    && authorization.generation <= status.retirement.cleanup_generation
            })
        {
            return Err(Error::Conflict);
        }
        if let Some(prior) = state.boots.get(&authorization.box_id) {
            if prior.authorization == authorization {
                let slot = self.boot_watch(&authorization.box_id)?;
                let mut watch = slot.lock().map_err(|_| Error::Unavailable)?;
                return if !watch.bound && watch.clock.live(Moment::now()?) {
                    Ok(authorization)
                } else {
                    Err(Error::Conflict)
                };
            }
            if prior.authorization.operation_id == authorization.operation_id
                || prior.bound_process.is_none()
            {
                return Err(Error::Conflict);
            }
        }
        if state.boots.len() >= MAX_RECORDS && !state.boots.contains_key(&authorization.box_id) {
            return Err(Error::Conflict);
        }
        if state.boots.values().any(|old| {
            old.authorization.box_id != authorization.box_id
                && (old.authorization.scoped_session_id == authorization.scoped_session_id
                    || old.authorization.operation_id == authorization.operation_id)
        }) || state.leases.values().any(|old| {
            old.box_id != authorization.box_id
                && old.scoped_session_id == authorization.scoped_session_id
        }) {
            return Err(Error::Conflict);
        }
        match state.leases.get(&authorization.box_id) {
            Some(prior) => {
                if Some(prior.revision) != authorization.prior_revision
                    || prior.generation != authorization.generation
                    || prior.scoped_session_id != authorization.scoped_session_id
                    || !prior.process.writers_exited()?
                {
                    return Err(Error::Conflict);
                }
                // Drain any in-flight old stop under its publication lock.
                // A reused Linux cgroup must never receive an old epoch's kill.
                let old = self.watch(&authorization.box_id)?;
                let mut old = old.lock().map_err(|_| Error::Unavailable)?;
                old.revoked = true;
                old.retired = true;
            }
            None if authorization.prior_revision.is_some() || state.leases.len() >= MAX_RECORDS => {
                return Err(Error::Conflict)
            }
            None => (),
        }
        let mut next = state.clone();
        next.boots.insert(authorization.box_id.clone(), record);
        let slot = Arc::new(Mutex::new(candidate));
        // Publish before uncertain persistence so a lost acknowledgement can
        // never leave an unmonitored native start attempt.
        self.boot_watches
            .write()
            .map_err(|_| Error::Unavailable)?
            .insert(authorization.box_id.clone(), slot.clone());
        self.commit(&mut state, next)?;
        let mut watch = slot.lock().map_err(|_| Error::Unavailable)?;
        if !watch.clock.live(Moment::now()?) || authorization.origin_current().is_err() {
            watch.clock.revoked = true;
            return Err(Error::Unavailable);
        }
        Ok(authorization)
    }

    pub fn boot_status(&self, id: &str) -> Result<Option<BootAuthorization>> {
        self.available()?;
        let state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        Ok(state
            .boots
            .get(id)
            .map(|record| record.authorization.clone()))
    }

    /// Call only after the owned SDK start task has settled. Binding confirms a
    /// live kernel writer, not permission to expose the session to public calls.
    pub fn bind_boot(
        &self,
        authorization: BootAuthorization,
        process: HostProcess,
    ) -> Result<ExecutionLease> {
        authorization.validate()?;
        process.verify_current_writer(&authorization.box_id)?;
        let discovered = authorization.executable.discover()?;
        if discovered.is_empty()
            || discovered
                .iter()
                .any(|writer| !same_writer(writer, &process))
        {
            return Err(Error::Conflict);
        }
        authorization.origin_current()?;
        let lease = authorization.lease(process.clone());
        lease.validate()?;
        let mut candidate = Watch::initial(&lease, Moment::now()?, false)?;
        if !candidate.live(Moment::now()?) {
            return Err(Error::Conflict);
        }
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        let record = state
            .boots
            .get(&authorization.box_id)
            .ok_or(Error::Conflict)?;
        if record.authorization != authorization {
            return Err(Error::Conflict);
        }
        let slot = self.boot_watch(&authorization.box_id)?;
        if let Some(bound) = &record.bound_process {
            if bound != &process
                || state.leases.get(&authorization.box_id) != Some(&lease)
                || !self
                    .watch(&authorization.box_id)?
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .live(Moment::now()?)
            {
                return Err(Error::Conflict);
            }
            return Ok(lease);
        }
        if !slot
            .lock()
            .map_err(|_| Error::Unavailable)?
            .clock
            .live(Moment::now()?)
        {
            return Err(Error::Conflict);
        }
        let mut next = state.clone();
        next.boots
            .get_mut(&authorization.box_id)
            .ok_or(Error::Unavailable)?
            .bound_process = Some(process.clone());
        next.leases
            .insert(authorization.box_id.clone(), lease.clone());
        let live = Arc::new(Mutex::new(candidate));
        self.watches
            .write()
            .map_err(|_| Error::Unavailable)?
            .insert(authorization.box_id.clone(), live.clone());
        self.commit(&mut state, next)?;
        // Boot expiry and publication serialize under the boot lock. No fsync
        // occurs under it; slow writes cannot postpone the pre-boot deadline.
        let mut boot = slot.lock().map_err(|_| Error::Unavailable)?;
        let mut running = live.lock().map_err(|_| Error::Unavailable)?;
        if !boot.clock.live(Moment::now()?)
            || !running.live(Moment::now()?)
            || process
                .verify_current_writer(&authorization.box_id)
                .is_err()
        {
            boot.clock.revoked = true;
            running.revoked = true;
            return Err(Error::Unavailable);
        }
        boot.bound = true;
        Ok(lease)
    }

    /// Run independently of known-writer expiry: executable/process discovery
    /// can perform filesystem queries and must not delay ordinary lease stops.
    pub fn enforce_boots(&self) -> Result<LeaseEnforcement> {
        let slots: Vec<_> = self
            .boot_watches
            .read()
            .map_err(|_| Error::Unavailable)?
            .values()
            .cloned()
            .collect();
        let mut result = LeaseEnforcement::default();
        for slot in slots {
            let authorization = {
                let boot = slot.lock().map_err(|_| Error::Unavailable)?;
                if boot.bound {
                    continue;
                }
                boot.authorization.clone()
            };
            // Never hold a publication lock while inspecting the executable.
            let discovered = authorization.executable.discover();
            let mut boot = slot.lock().map_err(|_| Error::Unavailable)?;
            if boot.bound {
                continue;
            }
            match discovered {
                Ok(writers) => {
                    for process in writers {
                        if !boot.known.contains(&process) {
                            boot.known.push(process);
                        }
                    }
                }
                Err(_) => {
                    boot.clock.revoked = true;
                    result.failed += 1;
                }
            }
        }
        let stopped = self.enforce_known_boots()?;
        result.attempted += stopped.attempted;
        result.failed += stopped.failed;
        Ok(result)
    }
    pub(super) fn enforce_known_boots(&self) -> Result<LeaseEnforcement> {
        let slots: Vec<_> = self
            .boot_watches
            .read()
            .map_err(|_| Error::Unavailable)?
            .values()
            .cloned()
            .collect();
        let mut targets = Vec::new();
        for slot in slots {
            let mut boot = slot.lock().map_err(|_| Error::Unavailable)?;
            if boot.bound {
                continue;
            }
            let expired = match Moment::now() {
                Ok(now) => !boot.clock.live(now),
                Err(_) => true,
            };
            if expired || self.poisoned.load(Ordering::SeqCst) {
                boot.clock.revoked = true;
                targets.extend(
                    boot.known
                        .iter()
                        .cloned()
                        .map(|process| (slot.clone(), process)),
                );
            }
        }
        let mut result = LeaseEnforcement::default();
        if !targets.is_empty() {
            let start = self.boot_cursor.fetch_add(64, Ordering::SeqCst) % targets.len();
            for offset in 0..targets.len().min(64) {
                let (slot, process) = &targets[(start + offset) % targets.len()];
                let boot = slot.lock().map_err(|_| Error::Unavailable)?;
                if boot.bound {
                    continue;
                }
                result.attempted += 1;
                if process.request_writer_stop().is_err() {
                    result.failed += 1;
                }
            }
        }
        Ok(result)
    }

    fn boot_watch(&self, id: &str) -> Result<Arc<Mutex<BootWatch>>> {
        self.boot_watches
            .read()
            .map_err(|_| Error::Unavailable)?
            .get(id)
            .cloned()
            .ok_or(Error::Unavailable)
    }
    pub(super) fn boots_settled(&self) -> Result<bool> {
        for slot in self
            .boot_watches
            .read()
            .map_err(|_| Error::Unavailable)?
            .values()
        {
            // Even after origin death, an empty discovery is not no-writer
            // proof for a pending native start. Preserve the unknown outcome.
            if !slot.lock().map_err(|_| Error::Unavailable)?.bound {
                return Ok(false);
            }
        }
        Ok(true)
    }
}
fn same_writer(a: &HostProcess, b: &HostProcess) -> bool {
    #[cfg(target_os = "macos")]
    {
        a == b
    }
    #[cfg(target_os = "linux")]
    {
        a.writer_cgroup.is_some()
            && a.writer_cgroup == b.writer_cgroup
            && a.host_id == b.host_id
            && a.boot_id == b.boot_id
            && a.process_namespace == b.process_namespace
    }
    #[cfg(not(any(target_os = "macos", target_os = "linux")))]
    {
        let _ = (a, b);
        false
    }
}

#[cfg(test)]
#[path = "execution_boot_tests.rs"]
mod tests;
