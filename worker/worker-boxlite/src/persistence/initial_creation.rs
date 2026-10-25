//! Durable intent precedes SDK create; selecting an ID never grants execution.
use super::{Error, HostProcess, InitialBoot, Journal, LeaseGuardIdentity};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InitialCreation {
    pub operation_id: String,
    pub scoped_session_id: String,
    pub guard: LeaseGuardIdentity,
    pub origin: HostProcess,
    pub expires_unix_ms: i64,
    pub options_sha256: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub box_id: String,
}

impl InitialCreation {
    /// Canonical SDK option JSON binds every option without duplicating private
    /// environments or startup commands in this creation record.
    pub fn options_digest(options: &boxlite::BoxOptions) -> Result<String, Error> {
        let mut value = serde_json::to_value(options).map_err(|_| Error::Conflict)?;
        value.sort_all_objects();
        let bytes = serde_json::to_vec(&value).map_err(|_| Error::Conflict)?;
        if bytes.len() > 64 << 10 {
            return Err(Error::Conflict);
        }
        Ok(format!("{:x}", Sha256::digest(bytes)))
    }

    pub(super) fn validate(&self) -> Result<(), Error> {
        self.guard.validate()?;
        self.origin.validate()?;
        if !super::types::valid_scope(&self.scoped_session_id)
            || !super::types::valid_id(&self.operation_id)
            || self.expires_unix_ms <= 0
            || self.options_sha256.len() != 64
            || !self
                .options_sha256
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            || (!self.box_id.is_empty() && !super::types::valid_id(&self.box_id))
            || self.guard.process.host_id != self.origin.host_id
            || self.guard.process.boot_id != self.origin.boot_id
            || self.guard.process.process_namespace != self.origin.process_namespace
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }

    pub(super) fn matches_boot(&self, boot: &InitialBoot) -> bool {
        !self.box_id.is_empty()
            && self.operation_id == boot.authorization.operation_id
            && self.scoped_session_id == boot.authorization.scoped_session_id
            && self.guard == boot.guard
            && self.origin == boot.authorization.origin
            && self.expires_unix_ms == boot.authorization.expires_unix_ms
            && self.box_id == boot.authorization.box_id
    }
}

impl Journal {
    pub fn begin_initial_creation(&self, intent: InitialCreation) -> Result<(), Error> {
        intent.validate()?;
        if !intent.box_id.is_empty() {
            return Err(Error::Conflict);
        }
        self.transaction(|state| {
            let scope = &intent.scoped_session_id;
            if state.worker_id != intent.guard.worker_id
                || state.current.contains_key(scope)
                || state.probes.contains_key(scope)
                || state.adoptions.contains_key(scope)
                || state.initial_boots.contains_key(scope)
            {
                return Err(Error::Conflict);
            }
            if let Some(prior) = state.initial_creations.get(scope) {
                return if prior == &intent {
                    Ok(())
                } else {
                    Err(Error::Conflict)
                };
            }
            state.initial_creations.insert(scope.clone(), intent);
            Ok(())
        })
    }

    pub fn select_initial_creation(
        &self,
        expected: &InitialCreation,
        box_id: &str,
    ) -> Result<(), Error> {
        expected.validate()?;
        if !expected.box_id.is_empty() || !super::types::valid_id(box_id) {
            return Err(Error::Conflict);
        }
        self.transaction(|state| {
            let scope = &expected.scoped_session_id;
            let mut selected = expected.clone();
            selected.box_id = box_id.into();
            let prior = state.initial_creations.get(scope).ok_or(Error::Conflict)?;
            if prior == &selected {
                return Ok(());
            }
            if prior != expected
                || state.current.contains_key(scope)
                || state.probes.contains_key(scope)
                || state.adoptions.contains_key(scope)
                || state.initial_boots.contains_key(scope)
            {
                return Err(Error::Conflict);
            }
            state.initial_creations.insert(scope.clone(), selected);
            Ok(())
        })
    }
}
