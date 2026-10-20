//! Optional immutable shared-snapshot caches never carry execution ownership.
use super::*;
use std::path::PathBuf;

struct Entry {
    path: PathBuf,
    archive: worker_boxlite::persistence::Archive,
    size: i64,
    expires: i64,
    accessed: (i64, i64),
}

fn cacheable(op: &Operation) -> bool {
    op.verified
        && op.archive.is_some()
        && !op.artifact.is_empty()
        && match op.kind {
            Kind::Source => {
                op.phase == Phase::Finalized && op.snapshot_committed && op.remote_verified
            }
            // Console only restores shared committed references. Once selected
            // and verified, an active/canceled target's archive is an optional
            // copy; its writable native disk always remains a separate owner.
            Kind::Target => matches!(op.phase, Phase::Active | Phase::Canceled),
            _ => false,
        }
}

fn matches_cache(candidate: &Operation, target: &Operation, now: i64) -> bool {
    candidate.operation_id != target.operation_id
        && candidate.scoped_session_id == target.scoped_session_id
        && candidate.restore_spec == target.restore_spec
        && candidate.archive == target.archive
        && cacheable(candidate)
        && candidate.cache_until_unix_ms > now
}

impl NativeControlEngine {
    pub(super) async fn copy_cache(&self, action: &ActionContext, op: &Operation) -> Outcome<bool> {
        if self.config.cache_max_bytes == 0 {
            return Ok(false);
        }
        let archive = op.archive.as_ref().ok_or(ControlError::Conflict)?;
        let state = self.journal.snapshot().map_err(journal_error)?;
        let destination = self
            .journal
            .artifact_path(&op.scoped_session_id, &op.operation_id, op.generation)
            .map_err(journal_error)?;
        let now = now_ms()?;
        for candidate in state
            .operations
            .values()
            .filter(|candidate| matches_cache(candidate, op, now))
        {
            self.authorize(action).await?;
            let source = self
                .journal
                .artifact_path(
                    &candidate.scoped_session_id,
                    &candidate.operation_id,
                    candidate.generation,
                )
                .map_err(journal_error)?;
            match self
                .transfer
                .copy_cached_archive(
                    source.clone(),
                    archive,
                    destination.clone(),
                    action.cancel.clone(),
                )
                .await
            {
                Ok(copied) => {
                    drop(copied);
                    return Ok(true);
                }
                Err(TransferError::Integrity) => {
                    // Optional known shared copies may be removed without
                    // changing the committed reference or touching a VM root.
                    self.authorize(action).await?;
                    let _ = self.transfer.evict_cached_archive(source, archive).await;
                }
                Err(TransferError::Retry | TransferError::Local) => (),
            }
        }
        self.authorize(action).await?;
        Ok(false)
    }

    pub(super) fn retain_cache(&self, operation: &str) -> Outcome<()> {
        self.journal
            .update(operation, |op| {
                // Replayed cleanup/readiness ACKs do not extend retention forever.
                if op.cache_until_unix_ms == 0 && op.verified {
                    op.cache_until_unix_ms = now_ms()
                        .map_err(|_| JournalError::Unavailable)?
                        .saturating_add(self.config.cache_ttl.as_millis() as i64);
                }
                Ok(())
            })
            .map_err(journal_error)
    }

    /// Caller holds admission while making new reservations. The budget is
    /// soft if another copy holds an archive lock; reserve still charges every
    /// retained byte, so pins cannot silently increase staging admission.
    pub(super) async fn trim_cache(&self, budget: i64) -> Outcome<()> {
        trim_cache_files(&self.journal, &self.transfer, budget).await
    }

    pub(super) async fn reserve_with_cache(
        &self,
        action: &ActionContext,
        bytes: i64,
    ) -> Outcome<()> {
        self.trim_cache(self.config.cache_max_bytes).await?;
        self.authorize(action).await?;
        match self.reserve(action, bytes) {
            Err(ControlError::Quota) => {
                // Prefer reclaiming optional shared copies before closing the
                // source gate or rejecting a restore's local staging budget.
                self.trim_cache(0).await?;
                self.authorize(action).await?;
                self.reserve(action, bytes)
            }
            result => result,
        }
    }
}

async fn trim_cache_files(journal: &Journal, transfer: &Transfer, budget: i64) -> Outcome<()> {
    let state = journal.snapshot().map_err(journal_error)?;
    let mut entries = Vec::new();
    let mut total = 0_i64;
    let now = now_ms()?;
    for op in state.operations.values().filter(|op| cacheable(op)) {
        let path = journal
            .artifact_path(&op.scoped_session_id, &op.operation_id, op.generation)
            .map_err(journal_error)?;
        let meta = match std::fs::symlink_metadata(&path) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(_) => return Err(ControlError::Storage),
            Ok(meta) => meta,
        };
        // SAFETY: geteuid reads process identity.
        if !meta.is_file()
            || meta.mode() & 0o077 != 0
            || meta.nlink() > 2
            || meta.uid() != unsafe { libc::geteuid() }
        {
            return Err(ControlError::Storage);
        }
        let size = i64::try_from(meta.len()).map_err(|_| ControlError::Quota)?;
        total = total.checked_add(size).ok_or(ControlError::Quota)?;
        entries.push(Entry {
            path,
            archive: op.archive.clone().ok_or(ControlError::Conflict)?,
            size,
            expires: op.cache_until_unix_ms,
            accessed: (meta.atime(), meta.atime_nsec()),
        });
    }
    entries.sort_by_key(|entry| entry.accessed);
    for entry in entries {
        if entry.expires > now && total <= budget {
            continue;
        }
        match transfer
            .evict_cached_archive(entry.path, &entry.archive)
            .await
        {
            Ok(_) => total = total.saturating_sub(entry.size),
            Err(TransferError::Retry) => (),
            Err(error) => return Err(transfer_error(error)),
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;
    use worker_boxlite::persistence::{artifact_name, Archive};
    struct Fixture {
        root: PathBuf,
        journal: Journal,
    }
    impl Fixture {
        fn new() -> Self {
            let root = std::env::temp_dir()
                .join(format!("onlyboxes-cache-policy-{}", uuid::Uuid::new_v4()));
            let journal = Journal::open(&root, "cache-worker").unwrap();
            Self { root, journal }
        }
        fn spec() -> RestoreSpec {
            serde_json::from_value(serde_json::json!({"runtime":"boxlite", "runtime_version":"0.10.7",
                "platform":"linux/arm64", "format":"boxlite-archive-v1", "image":format!("image@sha256:{}", "a".repeat(64)),
                "cpus":1, "memory_mib":256, "max_processes":128})).unwrap()
        }
        fn file(&self, id: &str, data: &[u8], accessed: u64) -> PathBuf {
            let op = self.journal.snapshot().unwrap().operations[id].clone();
            let path = self
                .journal
                .artifact_path(&op.scoped_session_id, id, op.generation)
                .unwrap();
            std::fs::write(&path, data).unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
            File::open(&path)
                .unwrap()
                .set_times(
                    std::fs::FileTimes::new()
                        .set_accessed(std::time::UNIX_EPOCH + Duration::from_secs(accessed)),
                )
                .unwrap();
            path
        }
        fn target(&self, id: &str, phase: Phase, expiry: i64, accessed: u64) -> PathBuf {
            let scope = format!("obx:cache:{id}");
            let archive = Archive {
                snapshot_id: "shared-snapshot".into(),
                size_bytes: 4,
                expanded_bytes: 4096,
                format: "boxlite-archive-v1".into(),
                sha256: "a".repeat(64),
            };
            let op: Operation = serde_json::from_value(serde_json::json!({
                "operation_id":id, "scoped_session_id":scope, "generation":1, "kind":"target", "phase":"prepared",
                "restore_spec":Self::spec(), "archive":archive, "staging_bytes":8192,
                "artifact":artifact_name(&scope, id, 1), "cache_until_unix_ms":expiry,
            })).unwrap();
            self.journal.claim(op).unwrap();
            self.journal
                .update(id, |op| {
                    op.phase = Phase::Importing;
                    op.verified = true;
                    op.box_id = format!("box-{id}");
                    Ok(())
                })
                .unwrap();
            self.journal
                .update(id, |op| {
                    op.phase = Phase::Ready;
                    op.lease_expires_unix_ms = now_ms().unwrap() + 300_000;
                    Ok(())
                })
                .unwrap();
            if phase == Phase::Active {
                self.journal
                    .update(id, |op| {
                        op.phase = Phase::Active;
                        Ok(())
                    })
                    .unwrap();
            } else if phase == Phase::Canceled {
                self.journal
                    .update(id, |op| {
                        op.phase = Phase::Canceling;
                        Ok(())
                    })
                    .unwrap();
                self.journal
                    .update(id, |op| {
                        op.phase = Phase::Canceled;
                        Ok(())
                    })
                    .unwrap();
            }
            self.file(id, b"data", accessed)
        }
        fn source(&self, id: &str, committed: bool) -> PathBuf {
            let scope = format!("obx:cache:{id}");
            self.journal.claim(serde_json::from_value(serde_json::json!({
                "operation_id":id, "scoped_session_id":scope, "generation":1, "kind":"source", "phase":"preparing",
                "restore_spec":Self::spec(), "box_id":format!("box-{id}"), "staging_bytes":8192,
            })).unwrap()).unwrap();
            self.journal.update(id, |op| {
                op.source_process=Some(serde_json::from_value(serde_json::json!({"host_id":uuid::Uuid::new_v4().to_string(),
                    "boot_id":uuid::Uuid::new_v4().to_string(), "process_namespace":"host", "pid":42, "start_time":1})).unwrap());
                op.source_stop_confirmed=true; op.phase=Phase::Frozen; op.verified=true;
                op.artifact=artifact_name(&scope, id, 1);
                op.archive=Some(Archive { snapshot_id:id.into(), size_bytes:4, expanded_bytes:4096,
                    format:"boxlite-archive-v1".into(), sha256:"a".repeat(64) });
                op.cache_until_unix_ms=now_ms().unwrap()+300_000; Ok(())
            }).unwrap();
            if committed {
                self.journal
                    .update(id, |op| {
                        op.phase = Phase::Committed;
                        op.remote_verified = true;
                        op.snapshot_committed = true;
                        Ok(())
                    })
                    .unwrap();
                self.journal
                    .update(id, |op| {
                        op.phase = Phase::Finalized;
                        Ok(())
                    })
                    .unwrap();
            }
            self.file(id, b"data", 100)
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = self.journal.close();
            let _ = std::fs::remove_dir_all(&self.root);
        }
    }
    fn transfer() -> Transfer {
        Transfer::new(2, Duration::from_secs(1)).unwrap()
    }

    #[tokio::test]
    async fn ttl_and_lru_evict_shared_copies_but_never_ready_or_uncommitted_data() {
        let f = Fixture::new();
        let future = now_ms().unwrap() + 300_000;
        let old = f.target("old", Phase::Active, future, 100);
        let recent = f.target("recent", Phase::Canceled, future, 200);
        let expired = f.target("expired", Phase::Active, 1, 300);
        let ready = f.target("ready", Phase::Ready, 1, 10);
        let frozen = f.source("uncommitted", false);
        let committed = f.source("committed", true);
        trim_cache_files(&f.journal, &transfer(), 8).await.unwrap();
        assert!(!expired.exists());
        // Equal atimes have stable insertion ordering, so the source named
        // committed and old target are both older than the recent target.
        trim_cache_files(&f.journal, &transfer(), 4).await.unwrap();
        assert!(!old.exists() && !committed.exists() && recent.exists());
        trim_cache_files(&f.journal, &transfer(), 0).await.unwrap();
        assert!(!recent.exists());
        assert!(ready.exists() && frozen.exists());
    }

    #[tokio::test]
    async fn eviction_skips_pinned_cache_and_retains_the_lock_inode() {
        let f = Fixture::new();
        let path = f.target("pinned", Phase::Active, 1, 1);
        let lock_path = PathBuf::from(format!("{}.lock", path.display()));
        let lock = OpenOptions::new()
            .write(true)
            .read(true)
            .create_new(true)
            .mode(0o600)
            .open(&lock_path)
            .unwrap();
        lock.try_lock().unwrap();
        let inode = lock.metadata().unwrap().ino();
        trim_cache_files(&f.journal, &transfer(), 0).await.unwrap();
        assert!(path.exists());
        drop(lock);
        trim_cache_files(&f.journal, &transfer(), 0).await.unwrap();
        assert!(!path.exists());
        assert_eq!(std::fs::metadata(lock_path).unwrap().ino(), inode);
    }

    #[test]
    fn cache_match_requires_scope_spec_snapshot_and_unexpired_commit_evidence() {
        let f = Fixture::new();
        f.source("source", true);
        let source = f.journal.snapshot().unwrap().operations["source"].clone();
        let mut target = source.clone();
        target.operation_id = "restore".into();
        target.generation = 2;
        assert!(matches_cache(&source, &target, now_ms().unwrap()));
        let mut other = target.clone();
        other.scoped_session_id = "obx:another:source".into();
        assert!(!matches_cache(&source, &other, now_ms().unwrap()));
        let mut other = target.clone();
        other.restore_spec.as_mut().unwrap().memory_mib = 512;
        assert!(!matches_cache(&source, &other, now_ms().unwrap()));
        let valid_target = target.clone();
        let mut other = target;
        other.archive.as_mut().unwrap().snapshot_id = "other-snapshot".into();
        assert!(!matches_cache(&source, &other, now_ms().unwrap()));
        assert!(!matches_cache(&source, &source, now_ms().unwrap()));
        let mut expired = source.clone();
        expired.cache_until_unix_ms = 1;
        assert!(!matches_cache(&expired, &valid_target, now_ms().unwrap()));
        let mut uncommitted = source;
        uncommitted.snapshot_committed = false;
        assert!(!cacheable(&uncommitted));
    }
}
