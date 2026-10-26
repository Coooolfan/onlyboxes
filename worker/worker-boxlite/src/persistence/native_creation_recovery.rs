//! A pre-boot intent and an exited issuer authorize no writable execution.
use super::*;
use crate::persistence::{InitialCreation, LeaseGuardClient};
use std::collections::BTreeMap;

async fn no_boot_owner(intent: &InitialCreation, guard: &LeaseGuardClient, id: &str) -> Result<()> {
    if intent.guard != *guard.identity()
        || !intent
            .origin
            .has_exited()
            .map_err(super::cleanup::journal_error)?
    {
        return Err(NativeError::Conflict);
    }
    if !id.is_empty() {
        let (boot, lease, retirement) = tokio::try_join!(
            guard.boot_status(id),
            guard.status(id),
            guard.retirement_status(id)
        )
        .map_err(super::cleanup::journal_error)?;
        if boot.is_some() || lease.is_some() || retirement.is_some() {
            return Err(NativeError::Conflict);
        }
    }
    Ok(())
}

pub(in crate::persistence) async fn abandon_creations(
    home: &Path,
    journal: &Journal,
    guard: &LeaseGuardClient,
) -> Result<()> {
    let state = journal.snapshot().map_err(super::cleanup::journal_error)?;
    let expected: BTreeMap<_, _> = state
        .initial_creations
        .iter()
        .filter(|(scope, intent)| {
            !state.initial_boots.contains_key(*scope) && !intent.cleanup_complete
        })
        .map(|(scope, intent)| (scope.clone(), intent.clone()))
        .collect();
    if expected.is_empty() {
        return Ok(());
    }
    if fs::symlink_metadata(home.join("db/boxlite.db")).is_err() {
        return Err(NativeError::Unavailable);
    }
    // Exact-identity RPC proves this guard is still serving, even when SDK
    // create returned no ID. The durable pre-boot barrier and issuer exit,
    // rather than a missing lease or empty directory alone, deny late start.
    guard
        .status("initial-creation-recovery-check")
        .await
        .map_err(super::cleanup::journal_error)?;
    let mut planned = state.clone();
    for (scope, intent) in &expected {
        no_boot_owner(intent, guard, &intent.box_id).await?;
        planned
            .initial_creations
            .get_mut(scope)
            .ok_or(NativeError::Conflict)?
            .abandoned = true;
    }
    let allow_selection = expected
        .iter()
        .filter(|(_, intent)| !intent.abandoned && intent.box_id.is_empty())
        .map(|(scope, _)| scope.clone())
        .collect();
    let bindings = super::preflight::creation_recovery_bindings(home, &planned, &allow_selection)?;
    for (scope, intent) in &expected {
        if !intent.abandoned && !intent.box_id.is_empty() && !bindings.contains_key(scope) {
            return Err(NativeError::Conflict);
        }
        if let Some(id) = bindings.get(scope) {
            no_boot_owner(intent, guard, id).await?;
        }
    }
    journal
        .abandon_initial_creations(&expected, &bindings)
        .map_err(super::cleanup::journal_error)?;
    tracing::info!(
        pending = expected.len(),
        "unbooted initial creations pending cleanup"
    );
    Ok(())
}

pub(super) fn authorize_residue(
    home: &Path,
    state: &crate::persistence::JournalState,
    id: &str,
) -> Result<bool> {
    let intent = state
        .initial_creations
        .values()
        .find(|intent| intent.box_id == id);
    let Some(intent) = intent else {
        return Ok(false);
    };
    if !intent.abandoned {
        return Ok(false);
    }
    if intent.cleanup_complete
        || state.initial_boots.contains_key(&intent.scoped_session_id)
        || !intent
            .origin
            .has_exited()
            .map_err(super::cleanup::journal_error)?
    {
        return Err(NativeError::Conflict);
    }
    crate::persistence::boot_executable::validate_unstarted_directory(home, id)
        .map_err(super::cleanup::journal_error)?;
    Ok(true)
}

impl NativeRuntime {
    /// Startup owns all SDK work and admits no session until every accepted
    /// abandonment has completed. A failed remove remains replayable.
    pub async fn cleanup_abandoned_initial_creations(&self, journal: &Journal) -> Result<()> {
        let state = journal.snapshot().map_err(super::cleanup::journal_error)?;
        for intent in state
            .initial_creations
            .values()
            .filter(|intent| intent.abandoned && !intent.cleanup_complete)
        {
            let guard = self.execution_guard.as_ref().ok_or(NativeError::Conflict)?;
            no_boot_owner(intent, guard, &intent.box_id).await?;
            let name = terminal_name(&intent.scoped_session_id);
            if let Some(info) = self
                .runtime
                .get_info(&name)
                .await
                .map_err(|_| NativeError::Unavailable)?
            {
                let id = info.id.as_str();
                if intent.box_id != id
                    || info.status != BoxStatus::Configured
                    || info.pid.is_some()
                    || info.name.as_deref() != Some(&name)
                {
                    return Err(NativeError::Conflict);
                }
                let config = read_config_mode(&self.home, id, false)?;
                if crate::persistence::InitialCreation::options_digest(&config.options)
                    .map_err(super::cleanup::journal_error)?
                    != intent.options_sha256
                {
                    return Err(NativeError::Conflict);
                }
                crate::persistence::boot_executable::validate_unstarted_directory(&self.home, id)
                    .map_err(super::cleanup::journal_error)?;
                self.runtime
                    .remove(id, false)
                    .await
                    .map_err(|_| NativeError::Unavailable)?;
            }
            if !intent.box_id.is_empty() {
                crate::persistence::boot_executable::validate_unstarted_directory(
                    &self.home,
                    &intent.box_id,
                )
                .map_err(super::cleanup::journal_error)?;
                super::cleanup::remove_native_home(&self.home, &intent.box_id)?;
            }
            if self
                .runtime
                .get_info(&name)
                .await
                .map_err(|_| NativeError::Unavailable)?
                .is_some()
            {
                return Err(NativeError::Conflict);
            }
            no_boot_owner(intent, guard, &intent.box_id).await?;
            journal
                .complete_initial_creation_cleanup(intent)
                .map_err(super::cleanup::journal_error)?;
            tracing::info!("unbooted initial creation cleanup completed");
        }
        Ok(())
    }
}
