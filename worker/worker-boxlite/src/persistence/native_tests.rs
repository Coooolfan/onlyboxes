use super::*;
use std::os::unix::fs::{symlink, PermissionsExt};

struct Fixture {
    home: PathBuf,
    config: BoxConfig,
    info: BoxInfo,
}
impl Fixture {
    fn new() -> Self {
        let home = std::env::temp_dir().join(format!(
            "onlyboxes-native-inspection-{}",
            uuid::Uuid::new_v4()
        ));
        fs::create_dir(&home).unwrap();
        fs::set_permissions(&home, fs::Permissions::from_mode(0o700)).unwrap();
        let home = fs::canonicalize(home).unwrap();
        let id = "native_box_123";
        let name = terminal_name("obx:owner:session");
        fs::create_dir(home.join("db")).unwrap();
        fs::create_dir_all(home.join("boxes").join(id)).unwrap();
        let spec = RestoreSpec {
            runtime: "boxlite".into(),
            runtime_version: RUNTIME_VERSION.into(),
            platform: guest_platform().unwrap().into(),
            format: ARCHIVE_FORMAT.into(),
            image: format!("registry.test:5000/project/image@sha256:{}", "a".repeat(64)),
            memory_mib: 384,
            cpus: 2.0,
            max_processes: 128,
            user: String::new(),
            working_dir: String::new(),
            env: BTreeMap::from([("PRIVATE".into(), "private-value".into())]),
            entrypoint: vec!["sh".into(), "-lc".into()],
            command: vec!["sleep infinity".into()],
        };
        let mut options = options_for_spec(&spec).unwrap();
        options.rootfs = RootfsSpec::Image("registry.test:5000/project/image:mutable".into());
        let config = serde_json::from_value(serde_json::json!({
            "id": id,
            "name": name,
            "created_at": "2026-10-18T00:00:00Z",
            "container": {"id": "b".repeat(64)},
            "options": options,
            "engine_kind": "Libkrun",
            "box_home": home.join("boxes").join(id)
        }))
        .unwrap();
        let info = serde_json::from_value(serde_json::json!({
            "id": id,
            "name": name,
            "status": "running",
            "created_at": "2026-10-18T00:00:00Z",
            "last_updated": "2026-10-18T00:00:00Z",
            "pid": 123,
            "image": "registry.test:5000/project/image:mutable",
            "cpus": 2,
            "memory_mib": 384,
            "network": null,
            "labels": {},
            "auto_stop": 0,
            "auto_delete": 0,
            "auto_resume": false,
            "health_status": boxlite::litebox::HealthStatus::default(),
            "exit_code": null,
            "resolved_image": {
                "manifest_digest": format!("sha256:{}", "a".repeat(64)),
                "total_layer_size": 1024
            }
        }))
        .unwrap();
        let fixture = Self { home, config, info };
        fixture.save();
        fixture
    }
    fn save(&self) {
        let connection = Connection::open(self.home.join("db/boxlite.db")).unwrap();
        connection.execute_batch("CREATE TABLE IF NOT EXISTS schema_version(id INTEGER PRIMARY KEY,version INTEGER);INSERT OR REPLACE INTO schema_version VALUES(1,10);CREATE TABLE IF NOT EXISTS box_config(id TEXT PRIMARY KEY,name TEXT UNIQUE,json TEXT);").unwrap();
        connection
            .execute(
                "INSERT OR REPLACE INTO box_config(id,name,json) VALUES(?1,?2,?3)",
                rusqlite::params![
                    self.config.id.as_str(),
                    self.config.name,
                    serde_json::to_string(&self.config).unwrap()
                ],
            )
            .unwrap();
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.home);
    }
}

fn census_state(f: &Fixture) -> crate::persistence::JournalState {
    let db = Connection::open(f.home.join("db/boxlite.db")).unwrap();
    db.execute_batch(
        "CREATE TABLE box_state(id TEXT PRIMARY KEY,status TEXT,pid INTEGER,json TEXT);",
    )
    .unwrap();
    let state = boxlite::litebox::BoxState::new();
    db.execute(
        "INSERT INTO box_state VALUES(?1,?2,NULL,?3)",
        rusqlite::params![
            f.config.id.as_str(),
            state.status.as_str(),
            serde_json::to_string(&state).unwrap()
        ],
    )
    .unwrap();
    crate::persistence::JournalState {
        version: 1,
        worker_id: "worker".into(),
        current: Default::default(),
        operations: Default::default(),
        probes: Default::default(),
        adoptions: Default::default(),
        initial_boots: Default::default(),
    }
}

#[test]
fn startup_census_preserves_orphans_and_never_migrates_unsupported_schema() {
    let f = Fixture::new();
    let journal = census_state(&f);
    preflight_home(&f.home, &journal).unwrap();
    let orphan = f.home.join("boxes/unrecorded");
    fs::create_dir(&orphan).unwrap();
    fs::write(orphan.join("disk.qcow2"), b"unique").unwrap();
    assert_eq!(
        preflight_home(&f.home, &journal),
        Err(NativeError::Conflict)
    );
    assert_eq!(fs::read(orphan.join("disk.qcow2")).unwrap(), b"unique");
    fs::remove_dir_all(orphan).unwrap();
    let pending = f.config.box_home.join(".snapshot_pending");
    fs::write(&pending, b"preserve-native-snapshot-intent").unwrap();
    assert_eq!(
        preflight_home(&f.home, &journal),
        Err(NativeError::Conflict)
    );
    assert_eq!(
        fs::read(&pending).unwrap(),
        b"preserve-native-snapshot-intent"
    );
    fs::remove_file(pending).unwrap();
    let db = Connection::open(f.home.join("db/boxlite.db")).unwrap();
    db.execute("UPDATE schema_version SET version=9", [])
        .unwrap();
    assert_eq!(
        preflight_home(&f.home, &journal),
        Err(NativeError::Unsupported)
    );
    assert_eq!(
        db.query_row("SELECT version FROM schema_version", [], |row| row
            .get::<_, i64>(0))
            .unwrap(),
        9
    );
}

#[test]
fn startup_census_rejects_name_substitution_and_non_image_rootfs() {
    let mut f = Fixture::new();
    let mut journal = census_state(&f);
    journal
        .probes
        .insert("obx:owner:session".into(), f.config.id.to_string());
    preflight_home(&f.home, &journal).unwrap();
    journal
        .probes
        .insert("obx:owner:session".into(), "replacement-root".into());
    assert_eq!(
        preflight_home(&f.home, &journal),
        Err(NativeError::Conflict)
    );
    journal
        .probes
        .insert("obx:owner:session".into(), f.config.id.to_string());
    f.config.options.rootfs = RootfsSpec::RootfsPath("/unrelated-host-root".into());
    f.save();
    assert_eq!(
        preflight_home(&f.home, &journal),
        Err(NativeError::Unsupported)
    );
}

#[test]
fn startup_census_accepts_exact_non_detached_name_fences_but_no_guest_disk() {
    let mut f = Fixture::new();
    let scope = "obx:owner:session";
    let operation = "canceled-import";
    let name = target_name(scope, operation, 1);
    let spec = spec_from_config(&f.config, &f.info).unwrap();
    let op:crate::persistence::Operation=serde_json::from_value(serde_json::json!({
        "operation_id":operation,"scoped_session_id":scope,"generation":1,"kind":"target","phase":"canceled",
        "restore_spec":spec,"archive":{"snapshot_id":"shared","size_bytes":1,"sha256":"a".repeat(64),"expanded_bytes":1024,"format":ARCHIVE_FORMAT},
        "artifact":crate::persistence::artifact_name(scope,operation,1),"staging_bytes":8192,
        "name_fences":{name.clone():f.config.id.to_string()},
    })).unwrap();
    op.validate().unwrap();
    f.config.name = Some(name.clone());
    f.config.options = super::cleanup::fence_options(&op, &name).unwrap();
    f.save();
    assert!(!f.config.options.detach);
    let mut journal = census_state(&f);
    journal.current.insert(scope.into(), operation.into());
    journal.operations.insert(operation.into(), op);
    preflight_home(&f.home, &journal).unwrap();
    fs::create_dir(f.config.box_home.join("disks")).unwrap();
    let disk = f.config.box_home.join("disks/disk.qcow2");
    fs::write(&disk, b"preserve").unwrap();
    assert_eq!(
        preflight_home(&f.home, &journal),
        Err(NativeError::Conflict)
    );
    assert_eq!(fs::read(disk).unwrap(), b"preserve");
}

#[test]
fn immutable_metadata_and_actual_digest_define_the_spec() {
    let f = Fixture::new();
    let config = read_config(&f.home, f.config.id.as_str()).unwrap();
    let spec = spec_from_config(&config, &f.info).unwrap();
    assert_eq!(
        spec.image,
        format!("registry.test:5000/project/image@sha256:{}", "a".repeat(64))
    );
    assert_eq!(spec.memory_mib, 384);
    assert_eq!(spec.cpus, 2.0);
    assert_eq!(spec.max_processes, 128);
    assert_eq!(spec.env["PRIVATE"], "private-value");
}

#[test]
fn readonly_inspection_does_not_rewrite_native_configuration() {
    let f = Fixture::new();
    let before = fs::read(f.home.join("db/boxlite.db")).unwrap();
    read_config(&f.home, f.config.id.as_str()).unwrap();
    assert!(before == fs::read(f.home.join("db/boxlite.db")).unwrap());
}

#[test]
fn configuration_accepts_a_home_alias_but_not_an_escaping_box_parent() {
    let mut f = Fixture::new();
    let alias = f.home.with_extension("alias");
    symlink(&f.home, &alias).unwrap();
    f.config.box_home = alias.join("boxes").join(f.config.id.as_str());
    f.save();
    read_config(&f.home, f.config.id.as_str()).unwrap();
    // Configured name reservations have metadata but need no disk/home yet.
    fs::remove_dir_all(f.home.join("boxes").join(f.config.id.as_str())).unwrap();
    assert!(read_config(&f.home, f.config.id.as_str()).is_err());
    read_config_mode(&f.home, f.config.id.as_str(), false).unwrap();
    f.config.box_home = alias.join(f.config.id.as_str());
    f.save();
    assert!(read_config_mode(&f.home, f.config.id.as_str(), false).is_err());
    fs::remove_file(alias).unwrap();
}

#[test]
fn repository_pin_preserves_registry_port_and_replaces_tags_and_indexes() {
    let digest = format!("sha256:{}", "a".repeat(64));
    for (reference, repository) in [
        ("python:slim", "python"),
        (
            "registry:5000/project/image:tag",
            "registry:5000/project/image",
        ),
        ("registry:5000/image", "registry:5000/image"),
        ("python@sha256:old", "python"),
    ] {
        assert_eq!(
            pin_image(reference, &digest).unwrap(),
            format!("{repository}@{digest}")
        );
    }
    assert_eq!(
        pin_image("image", "sha256:bad"),
        Err(NativeError::Unsupported)
    );
}

#[test]
fn unsupported_options_are_rejected_instead_of_silently_lost() {
    let mutations: Vec<fn(&mut BoxOptions)> = vec![
        |o| o.rootfs = RootfsSpec::RootfsPath("/host/rootfs".into()),
        |o| o.detach = false,
        |o| o.auto_delete = Some(1),
        |o| o.disk_size_gb = Some(100),
        |o| o.tty = true,
        |o| o.advanced.privileged = true,
        |o| o.advanced.security.jailer_enabled = false,
        |o| o.advanced.security.sandbox_profile = Some("/host/profile".into()),
        |o| o.advanced.security.resource_limits.max_file_size = Some(1),
        |o| o.network = boxlite::NetworkSpec::Disabled,
        |o| o.env.push(("PRIVATE".into(), "duplicate".into())),
        |o| o.entrypoint = None,
        |o| o.cmd = None,
    ];
    for mutate in mutations {
        let mut f = Fixture::new();
        mutate(&mut f.config.options);
        assert!(matches!(
            spec_from_config(&f.config, &f.info),
            Err(NativeError::Unsupported | NativeError::Conflict)
        ));
    }
}

#[test]
fn native_id_name_limits_and_resolved_build_must_agree() {
    let changes: Vec<fn(&mut Fixture)> = vec![
        |f| f.info.name = Some("another-session".into()),
        |f| f.info.cpus = 3,
        |f| f.info.memory_mib = 512,
        |f| f.info.resolved_image = None,
        |f| f.info.image = "different-image".into(),
    ];
    for mutate in changes {
        let mut f = Fixture::new();
        mutate(&mut f);
        assert!(spec_from_config(&f.config, &f.info).is_err());
    }
}

#[test]
fn unknown_options_schema_and_oversized_native_metadata_are_rejected() {
    for mode in ["schema", "unknown-option", "oversized"] {
        let f = Fixture::new();
        let connection = Connection::open(f.home.join("db/boxlite.db")).unwrap();
        match mode {
            "schema" => {
                connection
                    .execute("UPDATE schema_version SET version=11", [])
                    .unwrap();
            }
            "oversized" => {
                connection
                    .execute(
                        "UPDATE box_config SET json=?1",
                        ["x".repeat(CONFIG_LIMIT + 1)],
                    )
                    .unwrap();
            }
            _ => {
                let mut value = serde_json::to_value(&f.config).unwrap();
                value["options"]["unknown_host_path"] = serde_json::json!("/host/private");
                connection
                    .execute("UPDATE box_config SET json=?1", [value.to_string()])
                    .unwrap();
            }
        }
        assert!(
            read_config(&f.home, f.config.id.as_str()).is_err(),
            "mode {mode}"
        );
    }
}

#[test]
fn native_database_and_box_directories_cannot_redirect_outside_the_home() {
    for mode in ["box-home", "db-symlink", "db-hardlink", "box-symlink"] {
        let mut f = Fixture::new();
        let outside = f.home.join("outside");
        fs::create_dir(&outside).unwrap();
        match mode {
            "box-home" => {
                f.config.box_home = outside;
                f.save();
            }
            "db-symlink" => {
                let path = f.home.join("db/boxlite.db");
                let moved = f.home.join("other.db");
                fs::rename(&path, &moved).unwrap();
                symlink(moved, path).unwrap();
            }
            "db-hardlink" => {
                fs::hard_link(f.home.join("db/boxlite.db"), f.home.join("other.db")).unwrap();
            }
            _ => {
                let path = f.home.join("boxes").join(f.config.id.as_str());
                fs::remove_dir(&path).unwrap();
                symlink(outside, path).unwrap();
            }
        }
        assert!(
            read_config(&f.home, f.config.id.as_str()).is_err(),
            "mode {mode}"
        );
    }
}

#[test]
fn options_factory_rejects_other_runtime_versions_and_platforms() {
    let f = Fixture::new();
    let mut spec = spec_from_config(&f.config, &f.info).unwrap();
    spec.runtime_version = "0.10.6".into();
    assert!(matches!(
        options_for_spec(&spec),
        Err(NativeError::Unsupported)
    ));
    spec.runtime_version = RUNTIME_VERSION.into();
    spec.platform = if spec.platform == "linux/arm64" {
        "linux/amd64".into()
    } else {
        "linux/arm64".into()
    };
    assert!(matches!(
        options_for_spec(&spec),
        Err(NativeError::Unsupported)
    ));
}

#[test]
fn another_vmm_engine_is_not_treated_as_the_same_native_runtime() {
    let mut f = Fixture::new();
    f.config.engine_kind = serde_json::from_value(serde_json::json!("Firecracker")).unwrap();
    assert!(matches!(
        spec_from_config(&f.config, &f.info),
        Err(NativeError::Unsupported)
    ));
}

#[test]
fn environment_order_is_preserved_semantically_without_duplicate_keys() {
    let mut f = Fixture::new();
    f.config.options.env = vec![("Z".into(), "last".into()), ("A".into(), "first".into())];
    let spec = spec_from_config(&f.config, &f.info).unwrap();
    assert_eq!(
        spec.env.keys().map(String::as_str).collect::<Vec<_>>(),
        vec!["A", "Z"]
    );
}

#[test]
fn imported_build_pin_requires_the_expected_immutable_image() {
    let mut f = Fixture::new();
    let spec = spec_from_config(&f.config, &f.info).unwrap();
    f.config.options.rootfs = RootfsSpec::Image(spec.image.clone());
    f.info.image = spec.image.clone();
    f.info.resolved_image = None;
    assert!(matches!(
        spec_from_config(&f.config, &f.info),
        Err(NativeError::Unsupported)
    ));
    assert!(spec_from_config_with_pin(&f.config, &f.info, Some(&spec)).unwrap() == spec);
    f.config.options.rootfs = RootfsSpec::Image("image:mutable".into());
    f.info.image = "image:mutable".into();
    assert!(matches!(
        spec_from_config_with_pin(&f.config, &f.info, Some(&spec)),
        Err(NativeError::Conflict)
    ));
}

#[test]
fn restore_names_bind_account_operation_and_generation() {
    let name = target_name("obx:owner:session", "restore-one", 2);
    assert_eq!(name, target_name("obx:owner:session", "restore-one", 2));
    for other in [
        target_name("obx:other:session", "restore-one", 2),
        target_name("obx:owner:session", "restore-two", 2),
        target_name("obx:owner:session", "restore-one", 3),
        terminal_name("obx:owner:session"),
    ] {
        assert_ne!(name, other);
    }
    assert!(name.len() <= 128);
    assert!(name.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-'));
}

#[test]
fn unacknowledged_import_disk_must_match_length_hash_and_owned_local_file() {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    let f = Fixture::new();
    let directory = f.config.box_home.join("disks");
    fs::create_dir(&directory).unwrap();
    let path = directory.join("disk.qcow2");
    let bytes = b"standalone-disk-content";
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&path)
        .unwrap();
    file.write_all(bytes).unwrap();
    drop(file);
    let hash = format!("sha256:{:x}", Sha256::digest(bytes));
    let verify =
        || import::verify_unselected_disk(&f.home, f.config.id.as_str(), bytes.len() as u64, &hash);
    assert_eq!(verify(), Ok(()));
    assert_eq!(
        import::verify_unselected_disk(
            &f.home,
            f.config.id.as_str(),
            bytes.len() as u64 + 1,
            &hash
        ),
        Err(NativeError::Conflict)
    );
    fs::write(&path, b"different--disk-content").unwrap();
    assert_eq!(verify(), Err(NativeError::Conflict));
    fs::write(&path, bytes).unwrap();
    let alias = f.home.join("alias");
    fs::hard_link(&path, &alias).unwrap();
    assert_eq!(verify(), Err(NativeError::Conflict));
    fs::remove_file(&alias).unwrap();
    fs::remove_file(&path).unwrap();
    fs::write(&alias, bytes).unwrap();
    symlink(&alias, &path).unwrap();
    assert_eq!(verify(), Err(NativeError::Unavailable));
    fs::remove_file(&path).unwrap();
    fs::remove_dir(&directory).unwrap();
    let outside = f.home.join("outside-disks");
    fs::create_dir(&outside).unwrap();
    fs::write(outside.join("disk.qcow2"), bytes).unwrap();
    symlink(&outside, &directory).unwrap();
    assert_eq!(verify(), Err(NativeError::Unsupported));
}

#[test]
fn native_unique_name_index_and_config_name_must_agree() {
    let f = Fixture::new();
    let connection = Connection::open(f.home.join("db/boxlite.db")).unwrap();
    connection
        .execute("UPDATE box_config SET name=?1", ["other-operation"])
        .unwrap();
    assert!(matches!(
        read_config(&f.home, f.config.id.as_str()),
        Err(NativeError::Conflict)
    ));
}

#[test]
fn source_pid_evidence_requires_the_exact_sdk_and_os_process_identity() {
    use crate::persistence::HostProcess;
    let f = Fixture::new();
    let record = HostProcess::capture(std::process::id()).unwrap();
    let path = f.config.box_home.join("shim.pid");
    fs::write(
        &path,
        format!("{}\n{}\nservices-mux-v1\n", record.pid, record.start_time),
    )
    .unwrap();
    #[cfg(target_os = "macos")]
    {
        let mut bound = record.clone();
        bound.bind_writer_group(f.config.id.as_str()).unwrap();
        assert_eq!(
            stop::read_shim_process(&f.home, f.config.id.as_str(), record.pid).unwrap(),
            bound
        );
    }
    // The test process is not a BoxLite cgroup; PID-only evidence is rejected.
    #[cfg(target_os = "linux")]
    assert_eq!(
        stop::read_shim_process(&f.home, f.config.id.as_str(), record.pid),
        Err(NativeError::Unavailable)
    );
    assert_eq!(
        stop::read_shim_process(&f.home, f.config.id.as_str(), record.pid + 1),
        Err(NativeError::Conflict)
    );
    fs::write(
        &path,
        format!("{}\n{}\n", record.pid, record.start_time + 1),
    )
    .unwrap();
    assert_eq!(
        stop::read_shim_process(&f.home, f.config.id.as_str(), record.pid),
        Err(NativeError::Conflict)
    );
    fs::write(&path, format!("{}\n", record.pid)).unwrap();
    assert_eq!(
        stop::read_shim_process(&f.home, f.config.id.as_str(), record.pid),
        Err(NativeError::Conflict)
    );
    fs::write(&path, b"bad\nrecord\n").unwrap();
    assert_eq!(
        stop::read_shim_process(&f.home, f.config.id.as_str(), record.pid),
        Err(NativeError::Unsupported)
    );
    fs::write(&path, vec![b'x'; 129]).unwrap();
    assert_eq!(
        stop::read_shim_process(&f.home, f.config.id.as_str(), record.pid),
        Err(NativeError::Unsupported)
    );
    fs::remove_file(&path).unwrap();
    let other = f.home.join("other-pid");
    fs::write(&other, format!("{}\n{}\n", record.pid, record.start_time)).unwrap();
    symlink(&other, &path).unwrap();
    assert_eq!(
        stop::read_shim_process(&f.home, f.config.id.as_str(), record.pid),
        Err(NativeError::Unavailable)
    );
}
