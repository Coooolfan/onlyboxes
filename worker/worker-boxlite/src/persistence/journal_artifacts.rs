//! Delete only artifacts derived from the permanently revoked scope's history.
use super::*;
use std::collections::BTreeSet;

impl Journal {
    /// Caller has joined all owned work. Transfer locks additionally prevent
    /// reclaiming files while a downloader still owns its private file handles.
    /// Empty lock inodes remain, so a stale waiter cannot split the lock domain.
    pub fn purge_session_artifacts(&self, operation: &str) -> Result<()> {
        let inner = self.inner()?;
        let op = inner
            .state
            .operations
            .get(operation)
            .ok_or(Error::Conflict)?;
        if op.kind != Kind::Delete
            || !matches!(op.phase, Phase::Deleting | Phase::Deleted)
            || inner
                .state
                .current
                .get(&op.scoped_session_id)
                .map(String::as_str)
                != Some(operation)
        {
            return Err(Error::Conflict);
        }
        let names: BTreeSet<_> = inner
            .state
            .operations
            .values()
            .filter(|prior| {
                prior.scoped_session_id == op.scoped_session_id
                    && matches!(prior.kind, Kind::Source | Kind::Target)
            })
            .map(|prior| {
                artifact_name(
                    &prior.scoped_session_id,
                    &prior.operation_id,
                    prior.generation,
                )
            })
            .collect();
        let root = self.root.join("artifacts");
        private_directory(&root)?;
        let mut locks = Vec::new();
        for name in &names {
            let lock = private_options()
                .read(true)
                .write(true)
                .create(true)
                .open(root.join(format!("{name}.lock")))
                .map_err(|_| Error::Unavailable)?;
            check_file(&lock, 0)?;
            lock.try_lock().map_err(|_| Error::Conflict)?;
            locks.push(lock);
        }
        let entries: Vec<_> = fs::read_dir(&root)
            .map_err(|_| Error::Unavailable)?
            .collect::<std::io::Result<Vec<_>>>()
            .map_err(|_| Error::Unavailable)?;
        let mut files = Vec::new();
        let mut directories = Vec::new();
        for entry in entries {
            let name = entry
                .file_name()
                .into_string()
                .map_err(|_| Error::Unavailable)?;
            for archive in &names {
                let stem = archive.strip_suffix(".boxlite").ok_or(Error::Conflict)?;
                let workspace = name.strip_prefix(&format!("work-{stem}-"));
                let temp = name
                    .strip_prefix(&format!("{archive}.partial."))
                    .and_then(|suffix| suffix.strip_suffix(".tmp"));
                let exact = name == *archive
                    || name == format!("{archive}.partial")
                    || name == format!("{archive}.partial.json");
                if !exact && workspace.is_none() && temp.is_none() {
                    continue;
                }
                if let Some(uuid) = workspace.or(temp) {
                    let parsed = uuid::Uuid::parse_str(uuid).map_err(|_| Error::Conflict)?;
                    if parsed.to_string() != uuid || parsed.is_nil() {
                        return Err(Error::Conflict);
                    }
                }
                let meta = fs::symlink_metadata(entry.path()).map_err(|_| Error::Unavailable)?;
                // SAFETY: geteuid reads process identity.
                if meta.uid() != unsafe { libc::geteuid() } || meta.mode() & 0o077 != 0 {
                    return Err(Error::Conflict);
                }
                if workspace.is_some() {
                    if !meta.is_dir() {
                        return Err(Error::Conflict);
                    }
                    directories.push(entry.path());
                } else {
                    if !meta.is_file() || meta.nlink() > 2 {
                        return Err(Error::Conflict);
                    }
                    if meta.nlink() == 2 {
                        let other = if name == *archive {
                            root.join(format!("{archive}.partial"))
                        } else if name == format!("{archive}.partial") {
                            root.join(archive)
                        } else {
                            return Err(Error::Conflict);
                        };
                        let paired = fs::symlink_metadata(other).map_err(|_| Error::Conflict)?;
                        if !paired.is_file()
                            || paired.ino() != meta.ino()
                            || paired.dev() != meta.dev()
                        {
                            return Err(Error::Conflict);
                        }
                    }
                    files.push(entry.path());
                }
                break;
            }
        }
        for path in files {
            fs::remove_file(path).map_err(|_| Error::Unavailable)?;
        }
        for path in directories {
            fs::remove_dir_all(path).map_err(|_| Error::Unavailable)?;
        }
        fs::File::open(root)
            .and_then(|file| file.sync_all())
            .map_err(|_| Error::Unavailable)?;
        drop(locks);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    struct Fixture {
        root: PathBuf,
        journal: Journal,
        archive: PathBuf,
    }
    impl Fixture {
        fn new() -> Self {
            let root = std::env::temp_dir()
                .join(format!("onlyboxes-artifact-purge-{}", uuid::Uuid::new_v4()));
            let journal = Journal::open(&root, "worker").unwrap();
            let spec = serde_json::json!({"runtime":"boxlite", "runtime_version":"0.10.7", "platform":"linux/arm64", "format":"boxlite-archive-v1", "image":format!("image@sha256:{}", "a".repeat(64)), "memory_mib":256, "cpus":1, "max_processes":128, "env":{}, "entrypoint":["sh"], "command":["sleep","infinity"]});
            journal.claim(serde_json::from_value(serde_json::json!({"operation_id":"source", "scoped_session_id":"obx:owner:session", "generation":1, "kind":"source", "phase":"preparing", "box_id":"source_box", "restore_spec":spec})).unwrap()).unwrap();
            let archive = journal
                .artifact_path("obx:owner:session", "source", 1)
                .unwrap();
            journal.claim(serde_json::from_value(serde_json::json!({"operation_id":"delete", "scoped_session_id":"obx:owner:session", "generation":2, "kind":"delete", "phase":"deleting", "restore_spec":spec})).unwrap()).unwrap();
            Self {
                root,
                journal,
                archive,
            }
        }
        fn write(&self, path: &Path) {
            let mut file = private_options()
                .write(true)
                .create_new(true)
                .open(path)
                .unwrap();
            file.write_all(b"private owned bytes").unwrap();
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.root);
        }
    }

    #[test]
    fn purge_requires_transfer_lock_and_keeps_the_same_lock_inode() {
        let f = Fixture::new();
        f.write(&f.archive);
        assert_eq!(
            f.journal.purge_session_artifacts("source"),
            Err(Error::Conflict)
        );
        let lock_path = f.archive.with_file_name(format!(
            "{}.lock",
            f.archive.file_name().unwrap().to_str().unwrap()
        ));
        let lock = private_options()
            .read(true)
            .write(true)
            .create(true)
            .open(&lock_path)
            .unwrap();
        lock.try_lock().unwrap();
        let inode = lock.metadata().unwrap().ino();
        assert_eq!(
            f.journal.purge_session_artifacts("delete"),
            Err(Error::Conflict)
        );
        assert!(f.archive.exists());
        drop(lock);
        f.journal.purge_session_artifacts("delete").unwrap();
        f.journal.purge_session_artifacts("delete").unwrap();
        assert!(!f.archive.exists());
        assert_eq!(fs::metadata(lock_path).unwrap().ino(), inode);
    }

    #[test]
    fn purge_removes_owned_partial_aliases_temps_and_workspaces_only() {
        let f = Fixture::new();
        f.write(&f.archive);
        let name = f.archive.file_name().unwrap().to_str().unwrap();
        let parent = f.archive.parent().unwrap();
        fs::hard_link(&f.archive, parent.join(format!("{name}.partial"))).unwrap();
        f.write(&parent.join(format!("{name}.partial.json")));
        f.write(&parent.join(format!("{name}.partial.{}.tmp", uuid::Uuid::new_v4())));
        let work = parent.join(format!(
            "work-{}-{}",
            name.strip_suffix(".boxlite").unwrap(),
            uuid::Uuid::new_v4()
        ));
        private_directory(&work).unwrap();
        f.write(&work.join("raw-private-archive"));
        let unrelated = parent.join(format!("{}.boxlite", "b".repeat(64)));
        f.write(&unrelated);
        let outside = f.root.join("unrelated");
        f.write(&outside);
        symlink(&outside, work.join("external-link")).unwrap();
        f.journal.purge_session_artifacts("delete").unwrap();
        assert_eq!(fs::read(&unrelated).unwrap(), b"private owned bytes");
        assert_eq!(fs::read(&outside).unwrap(), b"private owned bytes");
        assert!(!f.archive.exists());
        assert!(!work.exists());
        assert_eq!(fs::read_dir(parent).unwrap().count(), 2); // unrelated + retained lock
    }

    #[test]
    fn purge_rejects_redirected_archive_and_does_not_follow_links() {
        let f = Fixture::new();
        let outside = f.root.join("unrelated");
        f.write(&outside);
        symlink(&outside, &f.archive).unwrap();
        assert_eq!(
            f.journal.purge_session_artifacts("delete"),
            Err(Error::Conflict)
        );
        assert_eq!(fs::read(outside).unwrap(), b"private owned bytes");
        assert!(fs::symlink_metadata(&f.archive)
            .unwrap()
            .file_type()
            .is_symlink());
    }
}
