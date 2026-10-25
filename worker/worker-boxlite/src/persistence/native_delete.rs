//! Permanent deletion owns the whole local scope, including retained history.
use super::cleanup::{cleanup_operation, journal_error, require_exited, source_name};
use super::stop::read_shim_process;
use super::*;
use crate::persistence::{Error, HostProcess};
use std::collections::{BTreeMap, BTreeSet};

impl NativeRuntime {
    /// Snapshot-managed deletion requires Console's durable ownership fence.
    /// A never-managed initial root can instead carry validated local TTL
    /// expiry authority. Caller closes scope gates and joins owned work first.
    /// Never removes journal history or shared-store objects.
    pub async fn purge_session(&self, journal: Arc<Journal>, operation: &str) -> Result<()> {
        let (state, deletion) = cleanup_operation(&journal, operation)?;
        if deletion.kind != Kind::Delete
            || !matches!(deletion.phase, Phase::Deleting | Phase::Deleted)
        {
            return Err(NativeError::Conflict);
        }
        let history: Vec<_> = state
            .operations
            .values()
            .filter(|prior| {
                prior.scoped_session_id == deletion.scoped_session_id
                    && prior.operation_id != operation
            })
            .cloned()
            .collect();
        let mut names = BTreeSet::from([terminal_name(&deletion.scoped_session_id)]);
        let mut ids = deletion.cleanup_names.clone();
        let mut markers = deletion.name_fences.clone();
        let mut proofs: BTreeMap<String, Vec<HostProcess>> = BTreeMap::new();
        let mut needs_proof = BTreeSet::new();
        for prior in &history {
            if prior.kind != Kind::Cancel
                && (prior.generation >= deletion.generation
                    || prior.restore_spec != deletion.restore_spec)
            {
                return Err(NativeError::Conflict);
            }
            let name = match prior.kind {
                Kind::Source | Kind::Adopt => Some(source_name(&state, prior)?),
                Kind::Target => Some(target_name(
                    &prior.scoped_session_id,
                    &prior.operation_id,
                    prior.generation,
                )),
                _ => None,
            };
            if let Some(name) = name {
                names.insert(name.clone());
                if !prior.box_id.is_empty() {
                    insert_same(&mut ids, &prior.box_id, &name)?;
                    if prior.kind != Kind::Target
                        || prior.start_attempted
                        || matches!(prior.phase, Phase::Ready | Phase::Active)
                    {
                        needs_proof.insert(prior.box_id.clone());
                    }
                    for process in [&prior.source_process, &prior.started_process]
                        .into_iter()
                        .flatten()
                    {
                        proofs
                            .entry(prior.box_id.clone())
                            .or_default()
                            .push(process.clone());
                    }
                }
                for id in &prior.cleanup_box_ids {
                    insert_same(&mut ids, id, prior.cleanup_names.get(id).unwrap_or(&name))?;
                }
            }
            for (id, process) in &prior.cleanup_processes {
                proofs.entry(id.clone()).or_default().push(process.clone());
            }
            for (name, id) in &prior.name_fences {
                if !names.contains(name) {
                    return Err(NativeError::Conflict);
                }
                insert_same(&mut markers, name, id)?;
            }
        }
        if ids.values().any(|name| !names.contains(name))
            || markers.keys().any(|name| !names.contains(name))
        {
            return Err(NativeError::Conflict);
        }
        if !deletion.box_id.is_empty() && !ids.contains_key(&deletion.box_id) {
            ids.insert(
                deletion.box_id.clone(),
                terminal_name(&deletion.scoped_session_id),
            );
            needs_proof.insert(deletion.box_id.clone());
        }
        // Discover roots whose SDK acknowledgement was lost before ID pinning.
        // A legacy ordinary root may have run. A new operation-derived ID can
        // only be a stopped late import: task joining and deletion fencing forbid
        // a new controlled start after the generation has been revoked.
        for name in &names {
            if let Some(info) = self
                .runtime
                .get_info(name)
                .await
                .map_err(|_| NativeError::Unavailable)?
            {
                let mut marker = self.inspect_name_fence(&deletion, name, &info)?;
                for prior in &history {
                    if !marker {
                        marker = self.inspect_name_fence(prior, name, &info)?;
                    }
                }
                if !marker {
                    let id = info.id.as_str();
                    insert_same(&mut ids, id, name)?;
                    if *name == terminal_name(&deletion.scoped_session_id) {
                        needs_proof.insert(id.to_owned());
                    }
                }
            }
        }
        for (id, process) in &deletion.cleanup_processes {
            proofs.entry(id.clone()).or_default().push(process.clone());
        }
        journal
            .update(operation, |current| {
                for (id, name) in &ids {
                    current.cleanup_box_ids.insert(id.clone());
                    current.cleanup_names.insert(id.clone(), name.clone());
                }
                for (name, id) in &markers {
                    current.name_fences.insert(name.clone(), id.clone());
                }
                Ok(())
            })
            .map_err(journal_error)?;
        // Confirm every older recorded writer has exited before its disk can be
        // removed. The one live selected lifecycle is stopped by cleanup_named.
        for (id, name) in &ids {
            let info = self
                .runtime
                .get_info(id)
                .await
                .map_err(|_| NativeError::Unavailable)?;
            let running = if let Some(info) = info
                .as_ref()
                .filter(|info| info.status == BoxStatus::Running)
            {
                self.inspect_known(
                    &deletion.scoped_session_id,
                    id,
                    name,
                    deletion.restore_spec.as_ref(),
                )
                .await?;
                Some(read_shim_process(
                    &self.home,
                    id,
                    info.pid.ok_or(NativeError::Conflict)?,
                )?)
            } else {
                None
            };
            let retirement = if running.is_none() {
                self.retire_native_lease(&journal, operation, id).await?
            } else {
                None
            };
            let retired_process = retirement
                .as_ref()
                .map(|receipt| &receipt.retirement.process);
            let recorded = proofs.get(id).map(Vec::as_slice).unwrap_or_default();
            if running.is_none()
                && needs_proof.contains(id)
                && recorded.is_empty()
                && retirement.is_none()
            {
                return Err(NativeError::Conflict);
            }
            for process in recorded {
                if running.as_ref() != Some(process)
                    && retired_process != Some(process)
                    && !super::retirement::stopped_epoch(
                        &state,
                        &deletion.scoped_session_id,
                        id,
                        process,
                    )
                {
                    require_exited(process).await?;
                }
            }
        }
        for name in names {
            let (_, current) = cleanup_operation(&journal, operation)?;
            self.cleanup_named(&journal, &current, &name).await?;
        }
        let retained = journal.clone();
        let id = operation.to_owned();
        tokio::task::spawn_blocking(move || retained.purge_session_artifacts(&id))
            .await
            .map_err(|_| NativeError::Unavailable)?
            .map_err(journal_error)?;
        journal
            .update(operation, |current| {
                current.tombstone_box_id = current
                    .name_fences
                    .get(&terminal_name(&current.scoped_session_id))
                    .ok_or(Error::Conflict)?
                    .clone();
                current.phase = Phase::Deleted;
                Ok(())
            })
            .map_err(journal_error)
    }
}
fn insert_same(map: &mut BTreeMap<String, String>, key: &str, value: &str) -> Result<()> {
    if map.get(key).is_some_and(|old| old != value) {
        return Err(NativeError::Conflict);
    }
    map.insert(key.to_owned(), value.to_owned());
    Ok(())
}
