//! An initial adoption keeps its first process, service epoch and deadline.
//! Pending journal state grants no public execution authority.
use super::{Error, ExecutionLease, Journal, LeaseGuardIdentity};
use serde::{Deserialize, Serialize};
type Result<T> = std::result::Result<T, Error>;
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InitialAdoption {
    pub guard: LeaseGuardIdentity,
    pub lease: ExecutionLease,
    pub confirmed: bool,
}
impl InitialAdoption {
    pub(super) fn validate(&self) -> Result<()> {
        self.guard.validate()?;
        self.lease.validate()?;
        if self.lease.generation != 1
            || self.lease.revision != 1
            || self.lease.revoked
            || self.guard.process.host_id != self.lease.process.host_id
            || self.guard.process.boot_id != self.lease.process.boot_id
            || self.guard.process.process_namespace != self.lease.process.process_namespace
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }
    fn same_owner(&self, other: &Self) -> bool {
        self.guard == other.guard
            && self.lease.box_id == other.lease.box_id
            && self.lease.scoped_session_id == other.lease.scoped_session_id
            && self.lease.process == other.lease.process
    }
}
impl Journal {
    pub fn begin_initial_adoption(&self, requested: InitialAdoption) -> Result<InitialAdoption> {
        requested.validate()?;
        if requested.confirmed {
            return Err(Error::Conflict);
        }
        let mut accepted = None;
        self.transaction(|state| {
            let scope = &requested.lease.scoped_session_id;
            if requested.guard.worker_id != state.worker_id
                || state.current.contains_key(scope)
                || state.initial_boots.get(scope).is_some_and(|boot| {
                    !boot.matches_adoption(state.adoptions.get(scope).unwrap_or(&requested))
                })
                || state
                    .probes
                    .get(scope)
                    .is_some_and(|id| id != &requested.lease.box_id)
            {
                return Err(Error::Conflict);
            }
            if let Some(prior) = state.adoptions.get(scope) {
                if !prior.same_owner(&requested) {
                    return Err(Error::Conflict);
                }
                accepted = Some(prior.clone());
            } else {
                state
                    .probes
                    .insert(scope.clone(), requested.lease.box_id.clone());
                state.adoptions.insert(scope.clone(), requested.clone());
                accepted = Some(requested.clone());
            }
            Ok(())
        })?;
        accepted.ok_or(Error::Unavailable)
    }
    pub fn confirm_initial_adoption(&self, expected: &InitialAdoption) -> Result<()> {
        expected.validate()?;
        self.transaction(|state| {
            let scope = &expected.lease.scoped_session_id;
            if state.current.contains_key(scope) {
                return Err(Error::Conflict);
            }
            let current = state.adoptions.get_mut(scope).ok_or(Error::Conflict)?;
            if current.guard != expected.guard || current.lease != expected.lease {
                return Err(Error::Conflict);
            }
            current.confirmed = true;
            Ok(())
        })
    }
}
