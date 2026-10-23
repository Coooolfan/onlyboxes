//! A stopped standalone QCOW2 is already a complete native root disk. Preserve
//! its exact bytes, including sparse-tail layout, instead of flattening it again.
use super::*;

pub(crate) fn standalone_export(
    disk: &mut File,
    output: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    original: &BoxOptions,
    snapshot: &str,
    limits: ArchiveLimits,
) -> Result<Option<Archive>> {
    limits.validate()?;
    if !valid_id(snapshot) || !valid_scope(scope) {
        return Err(ArchiveError::Invalid);
    }
    let before = disk.metadata().map_err(|_| ArchiveError::Io)?;
    let target = output.metadata().map_err(|_| ArchiveError::Io)?;
    if !before.is_file()
        || !target.is_file()
        || (before.dev(), before.ino()) == (target.dev(), target.ino())
    {
        return Err(ArchiveError::Invalid);
    }
    disk.seek(SeekFrom::Start(0))
        .map_err(|_| ArchiveError::Io)?;
    let mut header = [0; 104];
    disk.read_exact(&mut header)
        .map_err(|_| ArchiveError::Invalid)?;
    if &header[..4] != b"QFI\xfb" {
        return Err(ArchiveError::Invalid);
    }
    // Only the SDK flattens an external backing chain. Never package one as a
    // supposedly portable disk, and never treat other validation errors as COW.
    if header[8..20].iter().any(|b| *b != 0) {
        return Ok(None);
    }
    let virtual_size = validate_qcow(&header, limits.max_expanded_bytes as u64)?;
    if header[60..72].iter().any(|b| *b != 0) || header[80..96].iter().any(|b| *b != 0) {
        return Err(ArchiveError::Invalid);
    }
    let expanded = before
        .len()
        .checked_add(virtual_size)
        .and_then(|n| n.checked_add(MANIFEST_LIMIT))
        .ok_or(ArchiveError::Budget)?;
    if expanded > limits.max_expanded_bytes as u64 {
        return Err(ArchiveError::Budget);
    }
    if !matches!(original.rootfs, RootfsSpec::Image(_)) {
        return Err(ArchiveError::Invalid);
    }
    let mut permitted = options_for_spec(spec).map_err(|_| ArchiveError::Invalid)?;
    permitted.rootfs = original.rootfs.clone();
    let mut actual = original.clone();
    actual.env.sort();
    permitted.env.sort();
    if serde_json::to_value(actual).map_err(|_| ArchiveError::Invalid)?
        != serde_json::to_value(permitted).map_err(|_| ArchiveError::Invalid)?
    {
        return Err(ArchiveError::Invalid);
    }
    disk.seek(SeekFrom::Start(0))
        .map_err(|_| ArchiveError::Io)?;
    let mut hash = Sha256::new();
    let read = io::copy(&mut (&mut *disk).take(before.len() + 1), &mut hash)
        .map_err(|_| ArchiveError::Io)?;
    if read != before.len() {
        return Err(ArchiveError::Invalid);
    }
    let checksum = format!("sha256:{:x}", hash.finalize());
    let manifest = serde_json::to_vec(&Manifest {
        version: 3,
        box_name: Some(terminal_name(scope)),
        image: spec.image.clone(),
        box_options: serde_json::to_value(
            options_for_spec(spec).map_err(|_| ArchiveError::Invalid)?,
        )
        .map_err(|_| ArchiveError::Invalid)?,
        guest_disk_checksum: String::new(),
        container_disk_checksum: checksum.clone(),
        exported_at: chrono::Utc::now().to_rfc3339(),
    })
    .map_err(|_| ArchiveError::Invalid)?;
    output.set_len(0).map_err(|_| ArchiveError::Io)?;
    output
        .seek(SeekFrom::Start(0))
        .map_err(|_| ArchiveError::Io)?;
    let mut bounded = BoundedWriter {
        output,
        remaining: limits.max_bytes as u64,
        exceeded: false,
        io_failed: false,
    };
    let encoded = (|| -> io::Result<()> {
        let encoder = zstd::Encoder::new(&mut bounded, 3)?;
        let mut builder = tar::Builder::new(encoder);
        append_header(
            &mut builder,
            b"manifest.json",
            manifest.len() as u64,
            &manifest[..],
        )?;
        disk.seek(SeekFrom::Start(0))?;
        let mut reader = HashReader {
            input: &mut *disk,
            hash: Sha256::new(),
        };
        append_header(&mut builder, DISK, before.len(), &mut reader)?;
        if format!("sha256:{:x}", reader.hash.finalize()) != checksum {
            return Err(io::Error::other("standalone disk changed during export"));
        }
        builder.into_inner()?.finish()?;
        Ok(())
    })();
    if bounded.exceeded {
        return Err(ArchiveError::Budget);
    }
    if bounded.io_failed {
        return Err(ArchiveError::Io);
    }
    encoded.map_err(|_| ArchiveError::Invalid)?;
    let after = disk.metadata().map_err(|_| ArchiveError::Io)?;
    if before.len() != after.len()
        || before.mtime() != after.mtime()
        || before.mtime_nsec() != after.mtime_nsec()
        || before.ctime() != after.ctime()
        || before.ctime_nsec() != after.ctime_nsec()
    {
        return Err(ArchiveError::Invalid);
    }
    bounded.output.sync_all().map_err(|_| ArchiveError::Io)?;
    let (size, hash) = file_identity(bounded.output, limits.max_bytes)?;
    let archive = Archive {
        snapshot_id: snapshot.into(),
        size_bytes: size,
        sha256: hash,
        expanded_bytes: expanded as i64,
        format: ARCHIVE_FORMAT.into(),
    };
    archive.validate().map_err(|_| ArchiveError::Invalid)?;
    verify_archive(bounded.output, spec, scope, &archive, limits)?;
    Ok(Some(archive))
}
