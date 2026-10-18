use super::*;
use std::collections::BTreeMap;
use std::fs::{self, OpenOptions};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};

const SCOPE: &str = "obx:owner:session";
struct Fixture {
    root: std::path::PathBuf,
    spec: RestoreSpec,
    original: BoxOptions,
    manifest: Manifest,
    disk: Vec<u8>,
}
impl Fixture {
    fn new() -> Self {
        let root = std::env::temp_dir().join(format!(
            "onlyboxes-boxlite-archive-{}",
            uuid::Uuid::new_v4()
        ));
        fs::create_dir(&root).unwrap();
        fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).unwrap();
        let spec = RestoreSpec {
            runtime: "boxlite".into(),
            runtime_version: crate::persistence::RUNTIME_VERSION.into(),
            platform: crate::persistence::guest_platform().unwrap().into(),
            format: ARCHIVE_FORMAT.into(),
            image: format!("image@sha256:{}", "a".repeat(64)),
            memory_mib: 384,
            cpus: 2.0,
            max_processes: 128,
            user: String::new(),
            working_dir: String::new(),
            env: BTreeMap::from([("PRIVATE".into(), "private-env".into())]),
            entrypoint: vec!["sh".into(), "-lc".into()],
            command: vec!["sleep infinity".into()],
        };
        let mut original = options_for_spec(&spec).unwrap();
        original.rootfs = RootfsSpec::Image("image:mutable".into());
        let mut disk = vec![0; 4096];
        disk[..4].copy_from_slice(b"QFI\xfb");
        disk[4..8].copy_from_slice(&3u32.to_be_bytes());
        disk[20..24].copy_from_slice(&16u32.to_be_bytes());
        disk[24..32].copy_from_slice(&(1u64 << 20).to_be_bytes());
        disk[100..104].copy_from_slice(&104u32.to_be_bytes());
        disk[104..109].copy_from_slice(b"state");
        let manifest = Manifest {
            version: 3,
            box_name: Some(terminal_name(SCOPE)),
            image: "image:mutable".into(),
            box_options: serde_json::to_value(&original).unwrap(),
            guest_disk_checksum: String::new(),
            container_disk_checksum: format!("sha256:{:x}", Sha256::digest(&disk)),
            exported_at: "2026-10-18T00:00:00Z".into(),
        };
        Self {
            root,
            spec,
            original,
            manifest,
            disk,
        }
    }
    fn file(&self, name: &str) -> File {
        OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(self.root.join(name))
            .unwrap()
    }
    fn raw(&self) -> File {
        let mut decoded = Vec::new();
        member(
            &mut decoded,
            b"manifest.json",
            tar::EntryType::Regular,
            &serde_json::to_vec(&self.manifest).unwrap(),
            None,
        );
        member(
            &mut decoded,
            DISK,
            tar::EntryType::Regular,
            &self.disk,
            None,
        );
        decoded.extend([0; 1024]);
        self.encoded("raw", &decoded)
    }
    fn encoded(&self, name: &str, decoded: &[u8]) -> File {
        let mut file = self.file(name);
        file.write_all(&zstd::encode_all(decoded, 3).unwrap())
            .unwrap();
        file.seek(SeekFrom::Start(0)).unwrap();
        file
    }
    fn canonical(&self) -> (File, Archive) {
        let mut raw = self.raw();
        let mut output = self.file("canonical");
        let descriptor = canonicalize_export(
            &mut raw,
            &mut output,
            &self.spec,
            SCOPE,
            &self.original,
            "snapshot-1",
            limits(),
        )
        .unwrap();
        (output, descriptor)
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}
fn limits() -> ArchiveLimits {
    ArchiveLimits {
        max_bytes: 8 << 20,
        max_expanded_bytes: 8 << 20,
    }
}
fn member(
    output: &mut Vec<u8>,
    name: &[u8],
    kind: tar::EntryType,
    data: &[u8],
    declared: Option<u64>,
) {
    let mut header = tar::Header::new_gnu();
    header.as_mut_bytes()[..name.len()].copy_from_slice(name);
    header.set_entry_type(kind);
    header.set_size(declared.unwrap_or(data.len() as u64));
    header.set_cksum();
    output.extend(header.as_bytes());
    output.extend(data);
    output.resize(output.len().div_ceil(512) * 512, 0);
}
fn manifest_value(file: &mut File) -> serde_json::Value {
    file.seek(SeekFrom::Start(0)).unwrap();
    let decoder = zstd::Decoder::new(file).unwrap();
    let mut archive = tar::Archive::new(decoder);
    for entry in archive.entries().unwrap() {
        let mut entry = entry.unwrap();
        if entry.path_bytes().as_ref() == b"manifest.json" {
            let mut data = Vec::new();
            entry.read_to_end(&mut data).unwrap();
            return serde_json::from_slice(&data).unwrap();
        }
    }
    panic!("missing manifest")
}

#[test]
fn canonical_export_pins_image_preserves_disk_and_verifies_descriptor() {
    let f = Fixture::new();
    let (mut file, descriptor) = f.canonical();
    verify_archive(&mut file, &f.spec, SCOPE, &descriptor, limits()).unwrap();
    let value = manifest_value(&mut file);
    assert_eq!(value["image"], f.spec.image);
    assert_eq!(value["box_options"]["rootfs"]["Image"], f.spec.image);
    assert_eq!(
        value["container_disk_checksum"],
        f.manifest.container_disk_checksum
    );
    assert_eq!(
        descriptor.expanded_bytes,
        (1 << 20) + 4096 + MANIFEST_LIMIT as i64
    );
}

#[test]
fn restore_descriptor_and_private_spec_cannot_be_substituted() {
    let f = Fixture::new();
    let (mut file, descriptor) = f.canonical();
    let mut wrong = descriptor.clone();
    wrong.sha256 = "b".repeat(64);
    assert_eq!(
        verify_archive(&mut file, &f.spec, SCOPE, &wrong, limits()),
        Err(ArchiveError::Invalid)
    );
    let mut wrong = descriptor.clone();
    wrong.size_bytes += 1;
    assert_eq!(
        verify_archive(&mut file, &f.spec, SCOPE, &wrong, limits()),
        Err(ArchiveError::Invalid)
    );
    let mut wrong = descriptor.clone();
    wrong.expanded_bytes += 1;
    assert_eq!(
        verify_archive(&mut file, &f.spec, SCOPE, &wrong, limits()),
        Err(ArchiveError::Invalid)
    );
    let mut spec = f.spec.clone();
    spec.memory_mib += 1;
    assert_eq!(
        verify_archive(&mut file, &spec, SCOPE, &descriptor, limits()),
        Err(ArchiveError::Invalid)
    );
    assert_eq!(
        verify_archive(
            &mut file,
            &f.spec,
            "obx:other:session",
            &descriptor,
            limits()
        ),
        Err(ArchiveError::Invalid)
    );
}

#[test]
fn source_options_and_supplied_spec_must_agree() {
    let f = Fixture::new();
    let mut raw = f.raw();
    let mut output = f.file("output");
    let mut spec = f.spec.clone();
    spec.env.insert("INJECTED".into(), "wrong".into());
    assert_eq!(
        canonicalize_export(
            &mut raw,
            &mut output,
            &spec,
            SCOPE,
            &f.original,
            "snapshot-1",
            limits()
        )
        .err(),
        Some(ArchiveError::Invalid)
    );
}

#[test]
fn target_host_profile_is_rebuilt_and_other_overrides_are_rejected() {
    let f = Fixture::new();
    let mut options = options_for_spec(&f.spec).unwrap();
    options.advanced.security.seccomp_enabled = !options.advanced.security.seccomp_enabled;
    options.advanced.security.new_pid_ns = !options.advanced.security.new_pid_ns;
    options.advanced.security.chroot_enabled = !options.advanced.security.chroot_enabled;
    let mut value = serde_json::to_value(&f.manifest).unwrap();
    value["image"] = serde_json::json!(f.spec.image);
    value["box_options"] = serde_json::to_value(options).unwrap();
    let mut decoded = Vec::new();
    member(
        &mut decoded,
        b"manifest.json",
        tar::EntryType::Regular,
        &serde_json::to_vec(&value).unwrap(),
        None,
    );
    member(&mut decoded, DISK, tar::EntryType::Regular, &f.disk, None);
    decoded.extend([0; 1024]);
    let mut input = f.encoded("foreign-host", &decoded);
    let descriptor = describe_archive(&mut input, &f.spec, SCOPE, "snapshot-1", limits()).unwrap();
    let mut output = f.file("local-host");
    prepare_import_archive(
        &mut input,
        &mut output,
        &f.spec,
        SCOPE,
        &descriptor,
        limits(),
    )
    .unwrap();
    assert!(
        manifest_value(&mut output)["box_options"]
            == serde_json::to_value(options_for_spec(&f.spec).unwrap()).unwrap()
    );
    value["box_options"]["advanced"]["security"]["jailer_enabled"] = serde_json::json!(false);
    let mut decoded = Vec::new();
    member(
        &mut decoded,
        b"manifest.json",
        tar::EntryType::Regular,
        &serde_json::to_vec(&value).unwrap(),
        None,
    );
    member(&mut decoded, DISK, tar::EntryType::Regular, &f.disk, None);
    decoded.extend([0; 1024]);
    let mut input = f.encoded("unsafe-host", &decoded);
    assert!(describe_archive(&mut input, &f.spec, SCOPE, "snapshot-1", limits()).is_err());
}

#[test]
fn byte_expansion_and_qcow_virtual_capacity_limits_are_enforced() {
    let f = Fixture::new();
    let (mut file, descriptor) = f.canonical();
    let mut small = limits();
    small.max_bytes = descriptor.size_bytes - 1;
    assert_eq!(
        verify_archive(&mut file, &f.spec, SCOPE, &descriptor, small),
        Err(ArchiveError::Budget)
    );
    let mut small = limits();
    small.max_expanded_bytes = 1000;
    assert_eq!(
        verify_archive(&mut file, &f.spec, SCOPE, &descriptor, small),
        Err(ArchiveError::Budget)
    );
    fs::remove_file(f.root.join("raw")).unwrap();
    let mut raw = f.raw();
    let mut output = f.file("overflow-output");
    let mut small = limits();
    small.max_bytes = raw.metadata().unwrap().len() as i64;
    small.max_expanded_bytes = 1;
    assert_eq!(
        canonicalize_export(
            &mut raw,
            &mut output,
            &f.spec,
            SCOPE,
            &f.original,
            "snapshot-1",
            small
        )
        .err(),
        Some(ArchiveError::Budget)
    );
}

#[test]
fn duplicate_unknown_traversing_and_nonfile_members_are_rejected() {
    for (name, kind) in [
        (b"manifest.json".as_slice(), tar::EntryType::Regular),
        (b"../escape".as_slice(), tar::EntryType::Regular),
        (b"/outside".as_slice(), tar::EntryType::Regular),
        (b"extra".as_slice(), tar::EntryType::Regular),
        (b"link".as_slice(), tar::EntryType::Symlink),
        (DISK, tar::EntryType::Link),
        (b"directory".as_slice(), tar::EntryType::Directory),
        (b"fifo".as_slice(), tar::EntryType::Fifo),
    ] {
        let f = Fixture::new();
        let mut decoded = Vec::new();
        member(
            &mut decoded,
            b"manifest.json",
            tar::EntryType::Regular,
            &serde_json::to_vec(&f.manifest).unwrap(),
            None,
        );
        member(&mut decoded, DISK, tar::EntryType::Regular, &f.disk, None);
        member(&mut decoded, name, kind, b"unexpected", None);
        decoded.extend([0; 1024]);
        let mut input = f.encoded("bad-member", &decoded);
        let mut output = f.file("output");
        assert!(canonicalize_export(
            &mut input,
            &mut output,
            &f.spec,
            SCOPE,
            &f.original,
            "snapshot-1",
            limits()
        )
        .is_err());
    }
}

#[test]
fn huge_pax_and_longname_headers_are_rejected_without_allocating_payloads() {
    for kind in [tar::EntryType::XHeader, tar::EntryType::GNULongName] {
        let f = Fixture::new();
        let mut decoded = Vec::new();
        member(&mut decoded, b"metadata", kind, &[], Some(1 << 50));
        decoded.extend([0; 1024]);
        let mut input = f.encoded("metadata-bomb", &decoded);
        assert_eq!(preflight(&mut input, limits()), Err(ArchiveError::Invalid));
    }
}

#[test]
fn disk_backing_external_data_encryption_and_corruption_flags_are_rejected() {
    let changes: Vec<fn(&mut Vec<u8>)> = vec![
        |d| d[0] = 0,
        |d| d[8..16].copy_from_slice(&4096u64.to_be_bytes()),
        |d| d[16..20].copy_from_slice(&12u32.to_be_bytes()),
        |d| d[32..36].copy_from_slice(&1u32.to_be_bytes()),
        |d| d[72..80].copy_from_slice(&4u64.to_be_bytes()),
        |d| d[72..80].copy_from_slice(&2u64.to_be_bytes()),
        |d| d[100..104].copy_from_slice(&512u32.to_be_bytes()),
    ];
    for change in changes {
        let mut f = Fixture::new();
        change(&mut f.disk);
        f.manifest.container_disk_checksum = format!("sha256:{:x}", Sha256::digest(&f.disk));
        let mut input = f.raw();
        let mut output = f.file("output");
        assert_eq!(
            canonicalize_export(
                &mut input,
                &mut output,
                &f.spec,
                SCOPE,
                &f.original,
                "snapshot-1",
                limits()
            )
            .err(),
            Some(ArchiveError::Invalid)
        );
    }
}

#[test]
fn disk_checksum_and_unknown_manifest_options_are_rejected() {
    for mode in ["checksum", "version", "unknown", "options"] {
        let f = Fixture::new();
        let mut value = serde_json::to_value(&f.manifest).unwrap();
        match mode {
            "checksum" => {
                value["container_disk_checksum"] =
                    serde_json::json!(format!("sha256:{}", "c".repeat(64)))
            }
            "version" => value["version"] = serde_json::json!(4),
            "unknown" => value["host_path"] = serde_json::json!("/private"),
            _ => value["box_options"]["injected_host_path"] = serde_json::json!("/host"),
        };
        let mut decoded = Vec::new();
        member(
            &mut decoded,
            b"manifest.json",
            tar::EntryType::Regular,
            &serde_json::to_vec(&value).unwrap(),
            None,
        );
        member(&mut decoded, DISK, tar::EntryType::Regular, &f.disk, None);
        decoded.extend([0; 1024]);
        let mut input = f.encoded("bad-manifest", &decoded);
        let mut output = f.file("output");
        assert!(
            canonicalize_export(
                &mut input,
                &mut output,
                &f.spec,
                SCOPE,
                &f.original,
                "snapshot-1",
                limits()
            )
            .is_err(),
            "mode {mode}"
        );
    }
}

#[test]
fn same_inode_output_never_truncates_the_input() {
    let f = Fixture::new();
    let mut input = f.raw();
    let initial = input.metadata().unwrap().len();
    let mut output = OpenOptions::new()
        .read(true)
        .write(true)
        .open(f.root.join("raw"))
        .unwrap();
    assert_eq!(
        canonicalize_export(
            &mut input,
            &mut output,
            &f.spec,
            SCOPE,
            &f.original,
            "snapshot-1",
            limits()
        )
        .err(),
        Some(ArchiveError::Invalid)
    );
    assert_eq!(input.metadata().unwrap().len(), initial);
}

#[test]
fn truncated_streams_missing_end_records_and_hidden_trailing_data_are_rejected() {
    for mode in ["truncated-zstd", "missing-end", "trailing"] {
        let f = Fixture::new();
        let mut decoded = Vec::new();
        member(
            &mut decoded,
            b"manifest.json",
            tar::EntryType::Regular,
            &serde_json::to_vec(&f.manifest).unwrap(),
            None,
        );
        member(&mut decoded, DISK, tar::EntryType::Regular, &f.disk, None);
        if mode != "missing-end" {
            decoded.extend([0; 1024]);
        }
        if mode == "trailing" {
            decoded.extend(b"hidden-data");
        }
        let mut input = f.encoded("damaged", &decoded);
        if mode == "truncated-zstd" {
            let len = input.metadata().unwrap().len();
            input.set_len(len - 8).unwrap();
        }
        assert!(preflight(&mut input, limits()).is_err(), "mode {mode}");
    }
}

#[test]
fn gnu_sparse_disks_are_streamed_with_a_bounded_map() {
    let mut f = Fixture::new();
    let mut disk = f.disk.clone();
    disk.resize(1 << 20, 0);
    f.manifest.container_disk_checksum = format!("sha256:{:x}", Sha256::digest(&disk));
    let mut decoded = Vec::new();
    member(
        &mut decoded,
        b"manifest.json",
        tar::EntryType::Regular,
        &serde_json::to_vec(&f.manifest).unwrap(),
        None,
    );
    let mut header = tar::Header::new_gnu();
    header.as_mut_bytes()[..DISK.len()].copy_from_slice(DISK);
    header.set_entry_type(tar::EntryType::GNUSparse);
    header.set_size(f.disk.len() as u64);
    let gnu = header.as_gnu_mut().unwrap();
    gnu.set_real_size(disk.len() as u64);
    gnu.sparse[0].set_offset(0);
    gnu.sparse[0].set_length(f.disk.len() as u64);
    gnu.sparse[1].set_offset(disk.len() as u64);
    gnu.sparse[1].set_length(0);
    header.set_cksum();
    decoded.extend(header.as_bytes());
    decoded.extend(&f.disk);
    decoded.extend([0; 1024]);
    let mut input = f.encoded("sparse", &decoded);
    let mut output = f.file("output");
    let archive = canonicalize_export(
        &mut input,
        &mut output,
        &f.spec,
        SCOPE,
        &f.original,
        "snapshot-1",
        limits(),
    )
    .unwrap();
    verify_archive(&mut output, &f.spec, SCOPE, &archive, limits()).unwrap();
}

#[test]
fn sparse_extension_maps_are_bounded_before_tar_allocates_them() {
    let f = Fixture::new();
    let mut decoded = Vec::new();
    member(
        &mut decoded,
        b"manifest.json",
        tar::EntryType::Regular,
        &serde_json::to_vec(&f.manifest).unwrap(),
        None,
    );
    let mut header = tar::Header::new_gnu();
    header.as_mut_bytes()[..DISK.len()].copy_from_slice(DISK);
    header.set_entry_type(tar::EntryType::GNUSparse);
    header.set_size(0);
    header.as_gnu_mut().unwrap().set_real_size(4096);
    header.as_gnu_mut().unwrap().set_is_extended(true);
    header.set_cksum();
    decoded.extend(header.as_bytes());
    for _ in 0..=SPARSE_EXTENSIONS {
        let mut extension = tar::GnuExtSparseHeader::new();
        extension.set_is_extended(true);
        decoded.extend(extension.as_bytes());
    }
    let mut input = f.encoded("map-bomb", &decoded);
    assert_eq!(preflight(&mut input, limits()), Err(ArchiveError::Budget));
}

#[test]
fn output_byte_budget_and_io_failures_are_distinct() {
    let f = Fixture::new();
    let mut file = f.file("bounded");
    {
        let mut writer = BoundedWriter {
            output: &mut file,
            remaining: 3,
            exceeded: false,
            io_failed: false,
        };
        assert!(writer.write_all(b"overflow").is_err());
        assert!(writer.exceeded && !writer.io_failed);
        assert_eq!(writer.output.metadata().unwrap().len(), 0);
    }
    drop(file);
    let mut readonly = File::open(f.root.join("bounded")).unwrap();
    let mut writer = BoundedWriter {
        output: &mut readonly,
        remaining: 100,
        exceeded: false,
        io_failed: false,
    };
    assert!(writer.write_all(b"denied").is_err());
    assert!(writer.io_failed && !writer.exceeded);
}

#[test]
fn an_operation_named_native_export_becomes_a_canonical_session_archive() {
    let mut fixture = Fixture::new();
    let name = crate::persistence::target_name(SCOPE, "restore-operation", 2);
    fixture.manifest.box_name = Some(name.clone());
    let mut raw = fixture.raw();
    let mut output = fixture.file("canonical-operation");
    assert!(matches!(
        canonicalize_export(
            &mut raw,
            &mut output,
            &fixture.spec,
            SCOPE,
            &fixture.original,
            "next-snapshot",
            limits()
        ),
        Err(ArchiveError::Invalid)
    ));
    let wrong = crate::persistence::target_name(SCOPE, "restore-operation", 3);
    assert!(matches!(
        canonicalize_named_export(
            &mut raw,
            &mut output,
            &fixture.spec,
            SCOPE,
            &fixture.original,
            &wrong,
            "next-snapshot",
            limits()
        ),
        Err(ArchiveError::Invalid)
    ));
    let descriptor = canonicalize_named_export(
        &mut raw,
        &mut output,
        &fixture.spec,
        SCOPE,
        &fixture.original,
        &name,
        "next-snapshot",
        limits(),
    )
    .unwrap();
    verify_archive(&mut output, &fixture.spec, SCOPE, &descriptor, limits()).unwrap();
    let scan = verified_scan(&mut output, &fixture.spec, SCOPE, &descriptor, limits()).unwrap();
    assert_eq!(scan.manifest.box_name, Some(terminal_name(SCOPE)));
    assert_eq!(
        scan.manifest.container_disk_checksum,
        fixture.manifest.container_disk_checksum
    );
}

#[test]
fn validated_import_identity_is_the_complete_standalone_disk() {
    let fixture = Fixture::new();
    let (mut input, descriptor) = fixture.canonical();
    let mut output = fixture.file("import");
    let (_, size, hash) = prepare_import_with_identity(
        &mut input,
        &mut output,
        &fixture.spec,
        SCOPE,
        &descriptor,
        limits(),
    )
    .unwrap();
    assert_eq!(size, fixture.disk.len() as u64);
    assert_eq!(hash, format!("sha256:{:x}", Sha256::digest(&fixture.disk)));
}
