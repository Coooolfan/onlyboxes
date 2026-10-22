//! First VM creation uses the same pre-boot authority as restore. Uncertain
//! starts retain their original identity and cannot be replaced by another VM.
use super::{BootAuthorization, Error, InitialAdoption, Journal, LeaseGuardIdentity};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InitialBoot {
    pub guard: LeaseGuardIdentity,
    pub authorization: BootAuthorization,
    pub managed: bool,
}
impl InitialBoot {
    pub(super) fn validate(&self) -> Result<(), Error> {
        self.guard.validate()?;
        self.authorization.validate()?;
        if self.authorization.generation != 1
            || self.authorization.prior_revision.is_some()
            || self.guard.process.host_id != self.authorization.origin.host_id
            || self.guard.process.boot_id != self.authorization.origin.boot_id
            || self.guard.process.process_namespace != self.authorization.origin.process_namespace
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }
    pub(super) fn matches_adoption(&self, adoption: &InitialAdoption) -> bool {
        self.guard == adoption.guard
            && self.authorization.box_id == adoption.lease.box_id
            && self.authorization.scoped_session_id == adoption.lease.scoped_session_id
            && self.authorization.expires_unix_ms == adoption.lease.expires_unix_ms
    }
}
impl Journal {
    pub fn begin_initial_boot(&self, boot: InitialBoot) -> Result<(), Error> {
        boot.validate()?;
        if boot.managed {
            return Err(Error::Conflict);
        }
        self.transaction(|state| {
            let scope = &boot.authorization.scoped_session_id;
            if state.worker_id != boot.guard.worker_id || state.current.contains_key(scope) {
                return Err(Error::Conflict);
            }
            if let Some(prior) = state.initial_boots.get(scope) {
                return if prior == &boot {
                    Ok(())
                } else {
                    Err(Error::Conflict)
                };
            }
            if state.probes.contains_key(scope) || state.adoptions.contains_key(scope) {
                return Err(Error::Conflict);
            }
            state
                .probes
                .insert(scope.clone(), boot.authorization.box_id.clone());
            state.initial_boots.insert(scope.clone(), boot);
            Ok(())
        })
    }
    pub fn manage_initial_boot(&self, scope: &str) -> Result<(), Error> {
        self.transaction(|state| {
            let Some(boot) = state.initial_boots.get_mut(scope) else {
                return Ok(());
            };
            if !state
                .adoptions
                .get(scope)
                .is_some_and(|a| a.confirmed && boot.matches_adoption(a))
            {
                return Err(Error::Conflict);
            }
            boot.managed = true;
            Ok(())
        })
    }
}
