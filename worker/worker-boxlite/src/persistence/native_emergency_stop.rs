//! Shutdown never relies on the SDK runtime's live-handle cache to find VMs.
//! Kernel stops preserve disks and do not produce cleanup/ownership receipts.
use super::*;
impl NativeRuntime {
    pub fn request_guarded_writer_stops(&self, journal: &Journal) -> Result<()> {
        let guard = self.execution_guard.as_ref().ok_or(NativeError::Conflict)?;
        let state = journal.snapshot().map_err(|_| NativeError::Unavailable)?;
        if state.worker_id != guard.identity().worker_id {
            return Err(NativeError::Conflict);
        }
        let mut writers = Vec::new();
        let mut uncertain = false;
        let mut pending = Vec::new();
        for (scope, boot) in &state.initial_boots {
            if !state.adoptions.contains_key(scope)
                && state.current.get(scope).is_none_or(|id| {
                    let op = &state.operations[id];
                    !op.terminal()
                        && (op.box_id == boot.authorization.box_id
                            || op.cleanup_box_ids.contains(&boot.authorization.box_id))
                })
            {
                pending.push(&boot.authorization.executable);
            }
        }
        for id in state.current.values() {
            let op = &state.operations[id];
            if !op.terminal() && op.started_process.is_none() {
                if let Some(executable) = &op.boot_executable {
                    pending.push(executable);
                }
            }
        }
        for (scope, adoption) in &state.adoptions {
            if state.current.get(scope).is_none_or(|id| {
                let op = &state.operations[id];
                !op.terminal()
                    && (op.box_id == adoption.lease.box_id
                        || op.cleanup_box_ids.contains(&adoption.lease.box_id))
            }) {
                writers.push(adoption.lease.process.clone());
            }
        }
        for id in state.current.values() {
            let op = &state.operations[id];
            if op.terminal() {
                continue;
            }
            writers.extend(op.started_process.iter().cloned());
            if !op.source_stop_confirmed {
                writers.extend(op.source_process.iter().cloned());
            }
            writers.extend(op.cleanup_processes.values().cloned());
        }
        let mut unique = BTreeMap::new();
        for writer in writers {
            match serde_json::to_vec(&writer) {
                Ok(key) => {
                    unique.insert(key, writer);
                }
                Err(_) => uncertain = true,
            }
        }
        for writer in unique.values() {
            if writer.writers_exited() == Ok(true) {
                continue;
            }
            if writer.request_writer_stop().is_err() {
                uncertain = true;
            }
        }
        // Known writers are stopped before potentially slow executable scans.
        // Unknown start outcomes still retain all evidence and receive no ACK.
        for executable in pending {
            match executable.discover() {
                Ok(found) => {
                    for writer in found {
                        if writer.writers_exited() != Ok(true)
                            && writer.request_writer_stop().is_err()
                        {
                            uncertain = true;
                        }
                    }
                }
                Err(_) => uncertain = true,
            }
        }
        if uncertain {
            return Err(NativeError::Unavailable);
        }
        Ok(())
    }
}
