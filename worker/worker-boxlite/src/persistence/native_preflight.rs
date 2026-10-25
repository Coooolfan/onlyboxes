//! Read-only census before the SDK can migrate, adopt or remove native data.
use super::*;
use crate::persistence::{HostProcess, JournalState};
use std::collections::{BTreeMap, BTreeSet};

pub(in crate::persistence) fn preflight_home(home: &Path, journal: &JournalState) -> Result<()> {
    scan_home(home, journal, false).map(|_| ())
}

pub(super) fn scan_home(
    home: &Path,
    journal: &JournalState,
    reconcile: bool,
) -> Result<Vec<String>> {
    // A recorded create may have completed without returning its ID. Until
    // explicit reconciliation binds that outcome, even an empty inventory
    // cannot authorize SDK initialization or another creation for the scope.
    if journal
        .initial_creations
        .keys()
        .any(|scope| !journal.initial_boots.contains_key(scope))
    {
        return Err(NativeError::Conflict);
    }
    let host = HostProcess::capture(std::process::id()).map_err(|_| NativeError::Unavailable)?;
    let mut expected = BTreeMap::new();
    let mut known_names = BTreeSet::new();
    let mut markers = BTreeMap::new();
    let creations: BTreeMap<_, _> = journal
        .initial_creations
        .values()
        .map(|intent| (intent.box_id.as_str(), intent))
        .collect();
    for boot in journal.initial_boots.values() {
        boot.authorization
            .executable
            .validate_home(home, &boot.authorization.box_id)
            .map_err(|_| NativeError::Conflict)?;
    }
    for adoption in journal.adoptions.values() {
        let process = &adoption.lease.process;
        if process.host_id != host.host_id
            || process.boot_id == host.boot_id
                && process.process_namespace != host.process_namespace
        {
            return Err(NativeError::Conflict);
        }
    }
    for op in journal.operations.values() {
        if let Some(executable) = &op.boot_executable {
            executable
                .validate_home(home, &op.box_id)
                .map_err(|_| NativeError::Conflict)?;
        }
        for process in op
            .source_process
            .iter()
            .chain(op.started_process.iter())
            .chain(op.cleanup_processes.values())
        {
            if process.host_id != host.host_id
                || (process.boot_id == host.boot_id
                    && process.process_namespace != host.process_namespace)
            {
                return Err(NativeError::Conflict);
            }
        }
        let name = match op.kind {
            Kind::Source | Kind::Adopt => Some(super::cleanup::source_name(journal, op)?),
            Kind::Target => Some(target_name(
                &op.scoped_session_id,
                &op.operation_id,
                op.generation,
            )),
            _ => None,
        };
        if let Some(name) = name {
            known_names.insert(name.clone());
            if !op.box_id.is_empty() {
                insert(&mut expected, &op.box_id, &name)?;
            }
        }
        for (id, name) in &op.cleanup_names {
            insert(&mut expected, id, name)?;
            known_names.insert(name.clone());
        }
        for (name, id) in &op.name_fences {
            insert(&mut expected, id, name)?;
            insert(&mut markers, id, name)?;
            known_names.insert(name.clone());
        }
    }
    for op in journal
        .operations
        .values()
        .filter(|op| op.kind == Kind::Delete)
    {
        let name = terminal_name(&op.scoped_session_id);
        known_names.insert(name.clone());
        for id in [&op.box_id, &op.tombstone_box_id] {
            if !id.is_empty() && !expected.contains_key(id) {
                insert(&mut expected, id, &name)?;
            }
        }
    }
    for (scope, id) in &journal.probes {
        let name = terminal_name(scope);
        insert(&mut expected, id, &name)?;
        known_names.insert(name);
    }
    let boxes = home.join("boxes");
    let mut directories = BTreeSet::new();
    match fs::symlink_metadata(&boxes) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
        Err(_) => return Err(NativeError::Unavailable),
        Ok(_) => {
            checked_directory(&boxes)?;
            for entry in fs::read_dir(&boxes).map_err(|_| NativeError::Unavailable)? {
                let entry = entry.map_err(|_| NativeError::Unavailable)?;
                let id = entry
                    .file_name()
                    .into_string()
                    .map_err(|_| NativeError::Unsupported)?;
                if !valid_id(&id) {
                    return Err(NativeError::Unsupported);
                }
                checked_directory(&entry.path())?;
                // Native local-snapshot recovery can rewrite disk/backing paths.
                // The platform does not create these markers; preserve them for
                // explicit reconciliation instead of letting SDK startup act.
                match fs::symlink_metadata(entry.path().join(".snapshot_pending")) {
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => (),
                    _ => return Err(NativeError::Conflict),
                }
                directories.insert(id);
                if directories.len() > 16384 {
                    return Err(NativeError::Unsupported);
                }
            }
        }
    }
    let db = home.join("db/boxlite.db");
    match fs::symlink_metadata(&db) {
        Err(e)
            if e.kind() == std::io::ErrorKind::NotFound
                && directories.is_empty()
                && expected.is_empty() =>
        {
            return Ok(Vec::new())
        }
        Err(_) => return Err(NativeError::Unavailable),
        Ok(meta) => {
            // SAFETY: geteuid reads process identity.
            if !meta.is_file()
                || meta.nlink() != 1
                || meta.mode() & 0o022 != 0
                || meta.uid() != unsafe { libc::geteuid() }
            {
                return Err(NativeError::Unsupported);
            }
        }
    }
    checked_directory(&home.join("db"))?;
    for file in ["boxlite.db-wal", "boxlite.db-shm"] {
        match fs::symlink_metadata(home.join("db").join(file)) {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
            Err(_) => return Err(NativeError::Unavailable),
            Ok(meta) => {
                // SAFETY: geteuid reads process identity.
                if !meta.is_file()
                    || meta.nlink() != 1
                    || meta.mode() & 0o022 != 0
                    || meta.uid() != unsafe { libc::geteuid() }
                {
                    return Err(NativeError::Unsupported);
                }
            }
        }
    }
    let db = Connection::open_with_flags(
        db,
        OpenFlags::SQLITE_OPEN_READ_ONLY
            | OpenFlags::SQLITE_OPEN_NO_MUTEX
            | OpenFlags::SQLITE_OPEN_NOFOLLOW,
    )
    .map_err(|_| NativeError::Unavailable)?;
    db.busy_timeout(Duration::from_secs(1))
        .map_err(|_| NativeError::Unavailable)?;
    let tx = db
        .unchecked_transaction()
        .map_err(|_| NativeError::Unavailable)?;
    let version: i64 = tx
        .query_row("SELECT version FROM schema_version WHERE id=1", [], |row| {
            row.get(0)
        })
        .map_err(|_| NativeError::Unsupported)?;
    if version != 10 {
        return Err(NativeError::Unsupported);
    }
    let count: i64 = tx
        .query_row("SELECT COUNT(*) FROM box_config", [], |row| row.get(0))
        .map_err(|_| NativeError::Unavailable)?;
    if count > 16384 {
        return Err(NativeError::Unsupported);
    }
    let state_count: i64 = tx
        .query_row("SELECT COUNT(*) FROM box_state", [], |row| row.get(0))
        .map_err(|_| NativeError::Unsupported)?;
    let oversized:bool=tx.query_row("SELECT EXISTS(SELECT 1 FROM box_config WHERE length(CAST(id AS BLOB))>128 OR length(CAST(name AS BLOB))>128)",[],|row|row.get(0)).map_err(|_|NativeError::Unsupported)?;
    if state_count != count || oversized {
        return Err(NativeError::Unsupported);
    }
    let mut statement=tx.prepare("SELECT c.id,c.name,CASE WHEN length(CAST(c.json AS BLOB))<=65536 THEN c.json END,s.status,s.pid,CASE WHEN length(CAST(s.json AS BLOB))<=65536 THEN s.json END FROM box_config c LEFT JOIN box_state s ON s.id=c.id").map_err(|_| NativeError::Unavailable)?;
    let mut rows = statement.query([]).map_err(|_| NativeError::Unavailable)?;
    let mut ids = BTreeSet::new();
    let mut names = BTreeSet::new();
    while let Some(row) = rows.next().map_err(|_| NativeError::Unavailable)? {
        let id: String = row.get(0).map_err(|_| NativeError::Unsupported)?;
        let name: Option<String> = row.get(1).map_err(|_| NativeError::Unsupported)?;
        let config_data: String = row.get(2).map_err(|_| NativeError::Unsupported)?;
        let status: String = row.get(3).map_err(|_| NativeError::Unsupported)?;
        let pid: Option<u32> = row.get(4).map_err(|_| NativeError::Unsupported)?;
        let state_data: String = row.get(5).map_err(|_| NativeError::Unsupported)?;
        if !valid_id(&id) || config_data.len() > CONFIG_LIMIT || state_data.len() > CONFIG_LIMIT {
            return Err(NativeError::Unsupported);
        }
        let config: BoxConfig =
            serde_json::from_str(&config_data).map_err(|_| NativeError::Unsupported)?;
        let state: boxlite::litebox::BoxState =
            serde_json::from_str(&state_data).map_err(|_| NativeError::Unsupported)?;
        if config.id.as_str() != id
            || config.name != name
            || state.status.as_str() != status
            || state.pid != pid
            || config.box_home.file_name().and_then(|v| v.to_str()) != Some(&id)
            || fs::canonicalize(config.box_home.parent().ok_or(NativeError::Conflict)?)
                .map_err(|_| NativeError::Conflict)?
                != boxes
        {
            return Err(NativeError::Conflict);
        }
        if let Some(expected_name) = expected.get(&id) {
            if name.as_ref() != Some(expected_name) {
                return Err(NativeError::Conflict);
            }
        }
        if let Some(intent) = creations.get(id.as_str()) {
            if crate::persistence::InitialCreation::options_digest(&config.options)
                .map_err(|_| NativeError::Conflict)?
                != intent.options_sha256
            {
                return Err(NativeError::Conflict);
            }
        }
        // A protected terminal home has no untracked one-shot runtime. Its
        // Python boxes live in a separate home. Neither a familiar name nor
        // an unnamed SDK row proves ownership of a writable root.
        if name.is_none() {
            return Err(NativeError::Conflict);
        }
        if let Some(name) = name.as_deref() {
            if !names.insert(name.to_owned()) {
                return Err(NativeError::Conflict);
            }
            let known = expected.contains_key(&id);
            let pending_import = journal.operations.values().any(|op| {
                op.kind == Kind::Target
                    && op.box_id.is_empty()
                    && matches!(
                        op.phase,
                        super::Phase::Importing | super::Phase::Canceling | super::Phase::Canceled
                    )
                    && op.verified
                    && target_name(&op.scoped_session_id, &op.operation_id, op.generation) == name
                    && state.status == BoxStatus::Stopped
                    && pid.is_none()
            });
            let pending_marker = journal.operations.values().any(|op| {
                matches!(
                    op.phase,
                    super::Phase::Committed
                        | super::Phase::Finalized
                        | super::Phase::Canceling
                        | super::Phase::Canceled
                        | super::Phase::Deleting
                        | super::Phase::Deleted
                ) && known_names.contains(name)
                    && super::cleanup::fence_options(op, name).is_ok_and(|options| {
                        serde_json::to_value(options).ok()
                            == serde_json::to_value(&config.options).ok()
                    })
                    && matches!(state.status, BoxStatus::Configured | BoxStatus::Stopped)
                    && pid.is_none()
            });
            if !known && !pending_import && !pending_marker {
                return Err(NativeError::Conflict);
            }
            let recorded_marker = markers.contains_key(&id);
            if recorded_marker || pending_marker {
                let valid_options = journal.operations.values().any(|op| {
                    (pending_marker || op.name_fences.get(name) == Some(&id))
                        && super::cleanup::fence_options(op, name).is_ok_and(|options| {
                            serde_json::to_value(options).ok()
                                == serde_json::to_value(&config.options).ok()
                        })
                });
                if !valid_options
                    || pid.is_some()
                    || !matches!(state.status, BoxStatus::Configured | BoxStatus::Stopped)
                {
                    return Err(NativeError::Conflict);
                }
                for leaf in ["shim.pid", "disks/disk.qcow2"] {
                    match fs::symlink_metadata(config.box_home.join(leaf)) {
                        Err(error) if error.kind() == std::io::ErrorKind::NotFound => (),
                        _ => return Err(NativeError::Conflict),
                    }
                }
            } else if !matches!(config.options.rootfs, RootfsSpec::Image(_)) {
                return Err(NativeError::Unsupported);
            }
            if config.options.auto_delete != Some(0)
                || (!(recorded_marker || pending_marker) && !config.options.detach)
            {
                return Err(NativeError::Conflict);
            }
            if state.status.is_active() && !directories.contains(&id) {
                return Err(NativeError::Conflict);
            }
        }
        ids.insert(id);
    }
    if markers.keys().any(|id| !ids.contains(id)) {
        return Err(NativeError::Conflict);
    }
    let residual: Vec<_> = directories.difference(&ids).cloned().collect();
    for id in &residual {
        if !reconcile {
            return Err(NativeError::Conflict);
        }
        super::recovery::authorize_residue(home, journal, id)?;
    }
    Ok(residual)
}
fn insert(map: &mut BTreeMap<String, String>, id: &str, name: &str) -> Result<()> {
    if map.get(id).is_some_and(|stored| stored != name) {
        return Err(NativeError::Conflict);
    }
    map.insert(id.into(), name.into());
    Ok(())
}
fn checked_directory(path: &Path) -> Result<()> {
    let meta = fs::symlink_metadata(path).map_err(|_| NativeError::Unavailable)?;
    // SAFETY: geteuid reads process identity.
    if !meta.is_dir() || meta.mode() & 0o022 != 0 || meta.uid() != unsafe { libc::geteuid() } {
        return Err(NativeError::Unsupported);
    }
    Ok(())
}
