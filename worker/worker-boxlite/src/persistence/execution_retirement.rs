//! Exit evidence is durable before native deletion; scope ownership is released
//! only by an explicit completion carrying the exact accepted receipt.
use super::execution_lease::*;
use super::types::{valid_id, valid_scope};
use super::{Error, ExecutionLease, ExecutionLeaseLedger, HostProcess};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LeaseRetirement {
    pub operation_id: String,
    pub cleanup_generation: i64,
    pub box_id: String,
    pub scoped_session_id: String,
    pub generation: i64,
    pub revision: u64,
    pub process: HostProcess,
}
impl LeaseRetirement {
    pub(super) fn validate(&self) -> Result<()> {
        if !valid_id(&self.operation_id)
            || !valid_id(&self.box_id)
            || !valid_scope(&self.scoped_session_id)
            || self.generation <= 0
            || self.cleanup_generation < self.generation
            || self.revision == 0
            || self.revision >= 1 << 62
        {
            return Err(Error::Conflict);
        }
        self.process.validate_native_id(&self.box_id)
    }
    pub(super) fn matches(&self, lease: &ExecutionLease) -> bool {
        self.box_id == lease.box_id
            && self.scoped_session_id == lease.scoped_session_id
            && self.generation == lease.generation
            && self.revision == lease.revision
            && self.process == lease.process
    }
}
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RetirementStatus {
    pub retirement: LeaseRetirement,
    pub released: bool,
}
impl ExecutionLeaseLedger {
    pub fn retirement_status(&self, id: &str) -> Result<Option<RetirementStatus>> {
        self.available()?;
        let state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        Ok(state.retirements.get(id).cloned())
    }
    /// Caller has closed admission and joined native start/import tasks. A
    /// pending, unbound boot is never quiescent merely because no PID is found.
    pub fn prepare_retirement(&self, retirement: LeaseRetirement) -> Result<LeaseRetirement> {
        retirement.validate()?;
        self.available()?;
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        if let Some(prior) = state.retirements.get(&retirement.box_id) {
            return if prior.retirement == retirement {
                Ok(retirement)
            } else {
                Err(Error::Conflict)
            };
        }
        if state.retirements.len() >= MAX_RECORDS {
            return Err(Error::Conflict);
        }
        let lease = state
            .leases
            .get(&retirement.box_id)
            .ok_or(Error::Conflict)?;
        if !retirement.matches(lease)
            || state
                .boots
                .get(&retirement.box_id)
                .is_some_and(|boot| boot.bound_process.is_none())
        {
            return Err(Error::Conflict);
        }
        let slot = self.watch(&retirement.box_id)?;
        {
            let mut watch = slot.lock().map_err(|_| Error::Unavailable)?;
            if !retirement.process.writers_exited()? {
                return Err(Error::Conflict);
            }
            // Drain old stop delivery before the caller can delete/reuse paths.
            // There is no fsync under this publication lock.
            watch.revoked = true;
            watch.retired = true;
        }
        let mut next = state.clone();
        next.retirements.insert(
            retirement.box_id.clone(),
            RetirementStatus {
                retirement: retirement.clone(),
                released: false,
            },
        );
        self.commit(&mut state, next)?;
        Ok(retirement)
    }
    /// Caller confirms actual SDK/root cleanup. Persisted exit evidence, rather
    /// than a now-missing cgroup/PID file, proves the old writer has ended.
    pub fn finish_retirement(&self, retirement: LeaseRetirement) -> Result<LeaseRetirement> {
        retirement.validate()?;
        self.available()?;
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        self.available()?;
        let prior = state
            .retirements
            .get(&retirement.box_id)
            .ok_or(Error::Conflict)?;
        if prior.retirement != retirement {
            return Err(Error::Conflict);
        }
        if prior.released {
            return Ok(retirement);
        }
        if !state
            .leases
            .get(&retirement.box_id)
            .is_some_and(|lease| retirement.matches(lease))
        {
            return Err(Error::Conflict);
        }
        let mut next = state.clone();
        next.leases.remove(&retirement.box_id);
        next.boots.remove(&retirement.box_id);
        next.retirements
            .get_mut(&retirement.box_id)
            .ok_or(Error::Unavailable)?
            .released = true;
        self.commit(&mut state, next)?;
        self.watches
            .write()
            .map_err(|_| Error::Unavailable)?
            .remove(&retirement.box_id);
        self.boot_watches
            .write()
            .map_err(|_| Error::Unavailable)?
            .remove(&retirement.box_id);
        Ok(retirement)
    }
}

#[cfg(test)]
#[path = "execution_retirement_tests.rs"]
mod tests;
