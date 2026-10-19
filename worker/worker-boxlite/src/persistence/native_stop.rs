//! SDK metadata cannot prove quiescence after an uncertain stop. Pin host
//! lifecycle identity first and independently confirm exit before publication.
use super::*;
use crate::persistence::{HostProcess, Operation};
use std::fs::OpenOptions;
use std::io::Read;
use std::os::unix::fs::OpenOptionsExt;

impl NativeRuntime {
    /// Caller closes execution/preview admission, drains requests and serializes
    /// the scope. A failed or canceled call retains the root and durable process
    /// evidence. A stopped SDK record without that evidence is not adopted.
    pub async fn stop_source(&self, journal: &Journal, operation: &str) -> Result<NativeInstance> {
        let op = source_operation(journal, operation)?;
        let inspected = self.inspect_source(journal, operation).await?;
        if Some(&inspected.spec) != op.restore_spec.as_ref() {
            return Err(NativeError::Conflict);
        }
        if inspected.status == BoxStatus::Running {
            if op.source_stop_confirmed {
                return Err(NativeError::Conflict);
            }
            let process = read_shim_process(
                &self.home,
                &op.box_id,
                inspected.pid.ok_or(NativeError::Conflict)?,
            )?;
            journal
                .update(operation, |current| {
                    if current.phase != Phase::Preparing || current.kind != Kind::Source {
                        return Err(crate::persistence::Error::Conflict);
                    }
                    current.source_process = Some(process.clone());
                    Ok(())
                })
                .map_err(journal_error)?;
            let native = self
                .runtime
                .get(&op.box_id)
                .await
                .map_err(|_| NativeError::Unavailable)?
                .ok_or(NativeError::Missing)?;
            // Do not rely on the SDK's best-effort shutdown RPC to flush writes
            // completed before the source's admission gate was closed.
            let flush = async {
                let execution = native
                    .exec(boxlite::BoxCommand::new("python").args(["-c", "import os; os.sync()"]))
                    .await
                    .map_err(|_| NativeError::Unavailable)?;
                let result = execution
                    .wait()
                    .await
                    .map_err(|_| NativeError::Unavailable)?;
                if result.exit_code != 0
                    || result.error_message.as_ref().is_some_and(|m| !m.is_empty())
                {
                    return Err(NativeError::Unavailable);
                }
                Ok(())
            };
            tokio::time::timeout(Duration::from_secs(10), flush)
                .await
                .map_err(|_| NativeError::Unavailable)??;
            let current = source_operation(journal, operation)?;
            if current.source_process.as_ref() != Some(&process)
                || current.box_id != op.box_id
                || current.generation != op.generation
                || read_shim_process(&self.home, &op.box_id, process.pid)? != process
            {
                return Err(NativeError::Conflict);
            }
            native.stop().await.map_err(|_| NativeError::Unavailable)?;
        }
        let stopped = self.inspect_source(journal, operation).await?;
        if stopped.status != BoxStatus::Stopped
            || stopped.pid.is_some()
            || Some(&stopped.spec) != op.restore_spec.as_ref()
        {
            return Err(NativeError::Conflict);
        }
        confirm_source_exit(journal, operation, Duration::from_secs(10)).await?;
        Ok(stopped)
    }
}
fn source_operation(journal: &Journal, operation: &str) -> Result<Operation> {
    let state = journal.snapshot().map_err(journal_error)?;
    let op = state
        .operations
        .get(operation)
        .ok_or(NativeError::Conflict)?;
    if state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation)
        || op.kind != Kind::Source
        || !matches!(op.phase, Phase::Preparing | Phase::Frozen)
    {
        return Err(NativeError::Conflict);
    }
    Ok(op.clone())
}
fn journal_error(error: crate::persistence::Error) -> NativeError {
    match error {
        crate::persistence::Error::Conflict => NativeError::Conflict,
        crate::persistence::Error::Unavailable => NativeError::Unavailable,
    }
}

pub(in crate::persistence) async fn confirm_source_exit(
    journal: &Journal,
    operation: &str,
    wait: Duration,
) -> Result<()> {
    let op = source_operation(journal, operation)?;
    let process = op.source_process.ok_or(NativeError::Conflict)?;
    let deadline = tokio::time::Instant::now() + wait;
    while !process.has_exited().map_err(journal_error)? {
        if tokio::time::Instant::now() >= deadline {
            return Err(NativeError::Unavailable);
        }
        tokio::time::sleep_until(
            deadline.min(tokio::time::Instant::now() + Duration::from_millis(50)),
        )
        .await;
    }
    journal
        .update(operation, |current| {
            if current.kind != Kind::Source
                || current.generation != op.generation
                || current.box_id != op.box_id
                || current.source_process.as_ref() != Some(&process)
                || !matches!(current.phase, Phase::Preparing | Phase::Frozen)
            {
                return Err(crate::persistence::Error::Conflict);
            }
            current.source_stop_confirmed = true;
            Ok(())
        })
        .map_err(journal_error)
}

pub(super) fn read_shim_process(home: &Path, id: &str, pid: u32) -> Result<HostProcess> {
    // Validate ID, dedicated directories and immutable config before opening a
    // PID record. Do not use the SDK reader's error-to-`Absent` fallback.
    let _config = read_config(home, id)?;
    let mut file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(home.join("boxes").join(id).join("shim.pid"))
        .map_err(|_| NativeError::Unavailable)?;
    let meta = file.metadata().map_err(|_| NativeError::Unavailable)?;
    // SAFETY: geteuid reads the current process identity.
    if !meta.is_file()
        || meta.nlink() != 1
        || meta.mode() & 0o022 != 0
        || meta.uid() != unsafe { libc::geteuid() }
        || meta.len() > 128
    {
        return Err(NativeError::Unsupported);
    }
    let mut bytes = Vec::new();
    std::io::Read::by_ref(&mut file)
        .take(129)
        .read_to_end(&mut bytes)
        .map_err(|_| NativeError::Unavailable)?;
    if bytes.len() > 128 {
        return Err(NativeError::Unsupported);
    }
    let record = boxlite::util::PidRecord::decode(&bytes).map_err(|_| NativeError::Unsupported)?;
    if record.pid != pid || record.start_time.is_none() {
        return Err(NativeError::Conflict);
    }
    let process = HostProcess::capture(pid).map_err(journal_error)?;
    if Some(process.start_time) != record.start_time {
        return Err(NativeError::Conflict);
    }
    Ok(process)
}
