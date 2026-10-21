//! Resume database-first cleanup before SDK startup can prune native roots.
use super::*;
use crate::persistence::{HostProcess, JournalState};
use std::io::Read;
use std::os::unix::fs::OpenOptionsExt;

pub(in crate::persistence) fn reconcile_home(home: &Path, journal: &Journal) -> Result<()> {
    // Hold the durable operation/generation view through planning and deletion.
    // SDK initialization and new action admission remain outside this barrier.
    journal
        .with_state(|state| {
            let residual = super::preflight::scan_home(home, state, true)?;
            // Planning validates every residual before changing any directory. A
            // mixed unknown/live/uncommitted set is never partially reclaimed.
            for id in residual {
                authorize_residue(home, state, &id)?;
                super::cleanup::remove_native_home(home, &id)?;
            }
            super::preflight::preflight_home(home, state)
        })
        .map_err(super::cleanup::journal_error)?
}

pub(super) fn authorize_residue(home: &Path, state: &JournalState, id: &str) -> Result<()> {
    let mut owner = None;
    for current in state.current.values().map(|op| &state.operations[op]) {
        if !current.cleanup_box_ids.contains(id) {
            continue;
        }
        let name = current.cleanup_names.get(id).ok_or(NativeError::Conflict)?;
        let allowed = match current.kind {
            Kind::Source => {
                current.snapshot_committed
                    && current.remote_verified
                    && current.verified
                    && current.source_stop_confirmed
                    && matches!(current.phase, Phase::Committed | Phase::Finalized)
                    && name == &super::cleanup::source_name(state, current)?
            }
            Kind::Target => {
                matches!(current.phase, Phase::Canceling | Phase::Canceled)
                    && name
                        == &target_name(
                            &current.scoped_session_id,
                            &current.operation_id,
                            current.generation,
                        )
            }
            Kind::Delete => {
                matches!(current.phase, Phase::Deleting | Phase::Deleted)
                    && (name == &terminal_name(&current.scoped_session_id)
                        || state.operations.values().any(|op| {
                            op.kind == Kind::Target
                                && op.scoped_session_id == current.scoped_session_id
                                && op.generation < current.generation
                                && name
                                    == &target_name(
                                        &op.scoped_session_id,
                                        &op.operation_id,
                                        op.generation,
                                    )
                        }))
            }
            _ => false,
        };
        if !allowed || current.name_fences.values().any(|marker| marker == id) {
            return Err(NativeError::Conflict);
        }
        if owner.replace(current).is_some() {
            return Err(NativeError::Conflict);
        }
    }
    let owner = owner.ok_or(NativeError::Conflict)?;
    let history: Vec<_> = state
        .operations
        .values()
        .filter(|op| op.scoped_session_id == owner.scoped_session_id)
        .collect();
    // A cleanup ID cannot be borrowed from a different session's history.
    if state.operations.values().any(|op| {
        op.scoped_session_id != owner.scoped_session_id
            && (op.box_id == id
                || op.tombstone_box_id == id
                || op.cleanup_box_ids.contains(id)
                || op.name_fences.values().any(|marker| marker == id))
    }) {
        return Err(NativeError::Conflict);
    }
    let mut pins = Vec::new();
    let mut could_have_run = false;
    let mut unknown_boot = false;
    let mut unstarted_import = false;
    for op in &history {
        if let Some(process) = op.cleanup_processes.get(id) {
            pins.push(process);
        }
        if op.box_id == id {
            pins.extend(op.source_process.iter());
            pins.extend(op.started_process.iter());
            could_have_run |= matches!(op.kind, Kind::Source | Kind::Adopt)
                || op.start_attempted
                || matches!(op.phase, Phase::Ready | Phase::Active | Phase::RolledBack);
            unknown_boot |= op.start_attempted && op.started_process.is_none();
        }
        // A non-selected imported ID can never have received a controlled
        // start. Its operation-derived name and inspected cleanup intent bind
        // it even if import/remove acknowledgement was lost before ID pinning.
        if op.kind == Kind::Target
            && owner.cleanup_names.get(id)
                == Some(&target_name(
                    &op.scoped_session_id,
                    &op.operation_id,
                    op.generation,
                ))
            && (op.box_id != id || !op.start_attempted)
        {
            unstarted_import = true;
        }
    }
    if (could_have_run && pins.is_empty())
        || (unknown_boot && !owner.cleanup_processes.contains_key(id))
        || (!could_have_run && pins.is_empty() && !unstarted_import)
    {
        return Err(NativeError::Conflict);
    }
    for process in &pins {
        if !process
            .writers_exited()
            .map_err(super::cleanup::journal_error)?
        {
            return Err(NativeError::Conflict);
        }
    }
    check_pid_file(home, id, &pins)?;
    Ok(())
}

fn check_pid_file(home: &Path, id: &str, pins: &[&HostProcess]) -> Result<()> {
    let path = home.join("boxes").join(id).join("shim.pid");
    let mut file = match fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
    {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(NativeError::Conflict),
        Ok(file) => file,
    };
    let meta = file.metadata().map_err(|_| NativeError::Unavailable)?;
    // SAFETY: geteuid reads process identity.
    if !meta.is_file()
        || meta.nlink() != 1
        || meta.mode() & 0o022 != 0
        || meta.uid() != unsafe { libc::geteuid() }
        || meta.len() > 128
    {
        return Err(NativeError::Conflict);
    }
    let mut bytes = Vec::new();
    file.by_ref()
        .take(129)
        .read_to_end(&mut bytes)
        .map_err(|_| NativeError::Unavailable)?;
    if bytes.len() > 128 {
        return Err(NativeError::Conflict);
    }
    let record = boxlite::util::PidRecord::decode(&bytes).map_err(|_| NativeError::Conflict)?;
    if !pins
        .iter()
        .any(|process| process.pid == record.pid && Some(process.start_time) == record.start_time)
    {
        return Err(NativeError::Conflict);
    }
    Ok(())
}

#[cfg(test)]
#[path = "native_recovery_tests.rs"]
mod tests;
