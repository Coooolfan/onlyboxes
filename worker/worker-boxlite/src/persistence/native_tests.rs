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
        connection.execute_batch("CREATE TABLE IF NOT EXISTS schema_version(id INTEGER PRIMARY KEY,version INTEGER);INSERT OR REPLACE INTO schema_version VALUES(1,10);CREATE TABLE IF NOT EXISTS box_config(id TEXT PRIMARY KEY,json TEXT);").unwrap();
        connection
            .execute(
                "INSERT OR REPLACE INTO box_config(id,json) VALUES(?1,?2)",
                rusqlite::params![
                    self.config.id.as_str(),
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
