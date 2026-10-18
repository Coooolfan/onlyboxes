//! Bounded, streaming validation of native archives. No member is unpacked to
//! a caller-selected path, and source host security is never imported verbatim.

use super::native::options_for_spec;
use super::types::{valid_id, valid_scope};
use super::{terminal_name, Archive, RestoreSpec, ARCHIVE_FORMAT};
use boxlite::{BoxOptions, RootfsSpec};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::fs::File;
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::os::unix::fs::MetadataExt;

const MANIFEST_LIMIT: u64 = 64 << 10;
const TAR_OVERHEAD: u64 = 4 << 20;
const SPARSE_EXTENSIONS: usize = 1024;
const DISK: &[u8] = b"disk.qcow2";

#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum ArchiveError {
    #[error("BoxLite archive is invalid or does not match the restore specification")]
    Invalid,
    #[error("BoxLite archive exceeds its byte or expanded-data budget")]
    Budget,
    #[error("BoxLite archive I/O did not complete")]
    Io,
}
type Result<T> = std::result::Result<T, ArchiveError>;

#[derive(Clone, Copy)]
pub struct ArchiveLimits {
    pub max_bytes: i64,
    pub max_expanded_bytes: i64,
}
impl ArchiveLimits {
    pub fn validate(self) -> Result<()> {
        if self.max_bytes <= 0
            || self.max_expanded_bytes <= 0
            || self.max_bytes > 1 << 60
            || self.max_expanded_bytes > 1 << 60
        {
            return Err(ArchiveError::Budget);
        }
        Ok(())
    }
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Manifest {
    version: u32,
    box_name: Option<String>,
    image: String,
    box_options: serde_json::Value,
    guest_disk_checksum: String,
    container_disk_checksum: String,
    exported_at: String,
}
struct Scan {
    manifest: Manifest,
    disk_size: u64,
    expanded: i64,
}

enum Policy<'a> {
    Export {
        options: &'a BoxOptions,
        name: &'a str,
    },
    Committed,
}

/// The original options must come from the inspected, immutable native BoxConfig.
/// Only the manifest is rewritten; disk bytes and their checksum are preserved.
pub fn canonicalize_export(
    input: &mut File,
    output: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    original: &BoxOptions,
    snapshot: &str,
    limits: ArchiveLimits,
) -> Result<Archive> {
    canonicalize_named_export(
        input,
        output,
        spec,
        scope,
        original,
        &terminal_name(scope),
        snapshot,
        limits,
    )
}

/// The runtime has verified this exact name against native config and journal.
#[allow(clippy::too_many_arguments)]
pub(crate) fn canonicalize_named_export(
    input: &mut File,
    output: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    original: &BoxOptions,
    native_name: &str,
    snapshot: &str,
    limits: ArchiveLimits,
) -> Result<Archive> {
    rewrite(
        input,
        output,
        spec,
        scope,
        snapshot,
        limits,
        Policy::Export {
            options: original,
            name: native_name,
        },
    )
}

/// Verify the committed descriptor before producing a private local archive.
/// This replaces the source host's defaults with this host's security defaults.
pub fn prepare_import_archive(
    input: &mut File,
    output: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    expected: &Archive,
    limits: ArchiveLimits,
) -> Result<Archive> {
    prepare_import_with_identity(input, output, spec, scope, expected, limits).map(|v| v.0)
}

pub(crate) fn prepare_import_with_identity(
    input: &mut File,
    output: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    expected: &Archive,
    limits: ArchiveLimits,
) -> Result<(Archive, u64, String)> {
    let scan = verified_scan(input, spec, scope, expected, limits)?;
    let archive = rewrite(
        input,
        output,
        spec,
        scope,
        &expected.snapshot_id,
        limits,
        Policy::Committed,
    )?;
    Ok((
        archive,
        scan.disk_size,
        scan.manifest.container_disk_checksum,
    ))
}

pub fn verify_archive(
    input: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    expected: &Archive,
    limits: ArchiveLimits,
) -> Result<()> {
    verified_scan(input, spec, scope, expected, limits).map(|_| ())
}

fn verified_scan(
    input: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    expected: &Archive,
    limits: ArchiveLimits,
) -> Result<Scan> {
    limits.validate()?;
    if !valid_scope(scope) {
        return Err(ArchiveError::Invalid);
    }
    expected.validate().map_err(|_| ArchiveError::Invalid)?;
    let (size, hash) = file_identity(input, limits.max_bytes)?;
    if size != expected.size_bytes || hash != expected.sha256 {
        return Err(ArchiveError::Invalid);
    }
    let scan = scan(input, spec, scope, limits, Policy::Committed)?;
    if scan.expanded != expected.expanded_bytes {
        return Err(ArchiveError::Invalid);
    }
    Ok(scan)
}

fn rewrite(
    input: &mut File,
    output: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    snapshot: &str,
    limits: ArchiveLimits,
    policy: Policy<'_>,
) -> Result<Archive> {
    limits.validate()?;
    if !valid_id(snapshot) || !valid_scope(scope) {
        return Err(ArchiveError::Invalid);
    }
    let from = input.metadata().map_err(|_| ArchiveError::Io)?;
    let to = output.metadata().map_err(|_| ArchiveError::Io)?;
    if !to.is_file() || (from.dev() == to.dev() && from.ino() == to.ino()) {
        return Err(ArchiveError::Invalid);
    }
    file_identity(input, limits.max_bytes)?;
    let mut scan = scan(input, spec, scope, limits, policy)?;
    scan.manifest.box_name = Some(terminal_name(scope));
    scan.manifest.image = spec.image.clone();
    scan.manifest.box_options =
        serde_json::to_value(options_for_spec(spec).map_err(|_| ArchiveError::Invalid)?)
            .map_err(|_| ArchiveError::Invalid)?;
    let manifest = serde_json::to_vec(&scan.manifest).map_err(|_| ArchiveError::Invalid)?;
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
        input.seek(SeekFrom::Start(0))?;
        let decoder = zstd::Decoder::new(&mut *input)?;
        let mut archive = tar::Archive::new(decoder);
        let mut found = false;
        for entry in archive.entries()? {
            let mut entry = entry?;
            if entry.path_bytes().as_ref() == DISK {
                let mut reader = HashReader {
                    input: &mut entry,
                    hash: Sha256::new(),
                };
                append_header(&mut builder, DISK, scan.disk_size, &mut reader)?;
                if format!("sha256:{:x}", reader.hash.finalize())
                    != scan.manifest.container_disk_checksum
                {
                    return Err(io::Error::other("disk changed during archive rewrite"));
                }
                found = true;
            }
        }
        if !found {
            return Err(io::Error::other("disk missing during rewrite"));
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
    bounded.output.sync_all().map_err(|_| ArchiveError::Io)?;
    let (size, hash) = file_identity(bounded.output, limits.max_bytes)?;
    let result = Archive {
        snapshot_id: snapshot.to_owned(),
        size_bytes: size,
        sha256: hash,
        expanded_bytes: scan.expanded,
        format: ARCHIVE_FORMAT.into(),
    };
    result.validate().map_err(|_| ArchiveError::Invalid)?;
    Ok(result)
}

fn append_header<W: Write, R: Read>(
    builder: &mut tar::Builder<W>,
    name: &[u8],
    size: u64,
    reader: R,
) -> io::Result<()> {
    let mut header = tar::Header::new_gnu();
    header.set_size(size);
    header.set_mode(0o600);
    header.set_uid(0);
    header.set_gid(0);
    header.set_mtime(0);
    header.set_cksum();
    builder.append_data(
        &mut header,
        std::str::from_utf8(name).map_err(io::Error::other)?,
        reader,
    )
}

fn scan(
    input: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    limits: ArchiveLimits,
    policy: Policy<'_>,
) -> Result<Scan> {
    preflight(input, limits)?;
    input
        .seek(SeekFrom::Start(0))
        .map_err(|_| ArchiveError::Io)?;
    let decoder = zstd::Decoder::new(input).map_err(|_| ArchiveError::Invalid)?;
    let mut archive = tar::Archive::new(decoder);
    let mut manifest = None;
    let mut disk_size = 0;
    let mut disk_hash = String::new();
    let mut virtual_size = 0;
    for entry in archive.entries().map_err(|_| ArchiveError::Invalid)? {
        let mut entry = entry.map_err(|_| ArchiveError::Invalid)?;
        match entry.path_bytes().as_ref() {
            b"manifest.json" => {
                let mut data = Vec::new();
                entry
                    .take(MANIFEST_LIMIT + 1)
                    .read_to_end(&mut data)
                    .map_err(|_| ArchiveError::Invalid)?;
                if data.len() as u64 > MANIFEST_LIMIT {
                    return Err(ArchiveError::Budget);
                }
                manifest = Some(
                    serde_json::from_slice::<Manifest>(&data).map_err(|_| ArchiveError::Invalid)?,
                );
            }
            DISK => {
                disk_size = entry.size();
                let mut header = [0u8; 104];
                entry
                    .read_exact(&mut header)
                    .map_err(|_| ArchiveError::Invalid)?;
                virtual_size = validate_qcow(&header, limits.max_expanded_bytes as u64)?;
                let mut hash = Sha256::new();
                hash.update(header);
                let copied = io::copy(&mut entry, &mut hash).map_err(|_| ArchiveError::Invalid)?;
                if copied.checked_add(104) != Some(disk_size) {
                    return Err(ArchiveError::Invalid);
                }
                disk_hash = format!("sha256:{:x}", hash.finalize());
            }
            _ => return Err(ArchiveError::Invalid),
        }
    }
    let manifest = manifest.ok_or(ArchiveError::Invalid)?;
    let canonical_name = terminal_name(scope);
    let expected_name = match &policy {
        Policy::Export { name, .. } => *name,
        Policy::Committed => &canonical_name,
    };
    if manifest.version != 3
        || manifest.box_name.as_deref() != Some(expected_name)
        || !manifest.guest_disk_checksum.is_empty()
        || manifest.container_disk_checksum != disk_hash
        || manifest.exported_at.len() > 128
        || disk_size == 0
    {
        return Err(ArchiveError::Invalid);
    }
    let mut actual: BoxOptions =
        serde_json::from_value(manifest.box_options.clone()).map_err(|_| ArchiveError::Invalid)?;
    if serde_json::to_value(&actual).map_err(|_| ArchiveError::Invalid)? != manifest.box_options {
        return Err(ArchiveError::Invalid);
    }
    let RootfsSpec::Image(image) = &actual.rootfs else {
        return Err(ArchiveError::Invalid);
    };
    if *image != manifest.image {
        return Err(ArchiveError::Invalid);
    }
    actual.env.sort();
    let expected = match policy {
        Policy::Export {
            options: original, ..
        } => {
            let mut original = original.clone();
            original.env.sort();
            let mut permitted = options_for_spec(spec).map_err(|_| ArchiveError::Invalid)?;
            permitted.rootfs = original.rootfs.clone();
            if serde_json::to_value(&original).map_err(|_| ArchiveError::Invalid)?
                != serde_json::to_value(permitted).map_err(|_| ArchiveError::Invalid)?
            {
                return Err(ArchiveError::Invalid);
            }
            original
        }
        Policy::Committed => options_for_spec(spec).map_err(|_| ArchiveError::Invalid)?,
    };
    let actual = serde_json::to_value(&actual).map_err(|_| ArchiveError::Invalid)?;
    if actual != serde_json::to_value(&expected).map_err(|_| ArchiveError::Invalid)? {
        if !matches!(policy, Policy::Committed) {
            return Err(ArchiveError::Invalid);
        }
        let mut alternate = expected;
        alternate.advanced.security.seccomp_enabled = !alternate.advanced.security.seccomp_enabled;
        alternate.advanced.security.new_pid_ns = !alternate.advanced.security.new_pid_ns;
        alternate.advanced.security.chroot_enabled = !alternate.advanced.security.chroot_enabled;
        if actual != serde_json::to_value(alternate).map_err(|_| ArchiveError::Invalid)? {
            return Err(ArchiveError::Invalid);
        }
    }
    let expanded = disk_size
        .checked_add(virtual_size)
        .and_then(|n| n.checked_add(MANIFEST_LIMIT))
        .ok_or(ArchiveError::Budget)?;
    if expanded > limits.max_expanded_bytes as u64 {
        return Err(ArchiveError::Budget);
    }
    Ok(Scan {
        manifest,
        disk_size,
        expanded: expanded as i64,
    })
}

fn validate_qcow(header: &[u8; 104], limit: u64) -> Result<u64> {
    let version = u32::from_be_bytes(header[4..8].try_into().unwrap());
    let virtual_size = u64::from_be_bytes(header[24..32].try_into().unwrap());
    let cluster_bits = u32::from_be_bytes(header[20..24].try_into().unwrap());
    if &header[..4] != b"QFI\xfb"
        || !matches!(version, 2 | 3)
        || header[8..20].iter().any(|b| *b != 0)
        || header[32..36].iter().any(|b| *b != 0)
        || virtual_size == 0
        || !(9..=21).contains(&cluster_bits)
        || (version == 3
            && (header[72..80].iter().any(|b| *b != 0)
                || u32::from_be_bytes(header[100..104].try_into().unwrap()) != 104))
    {
        return Err(ArchiveError::Invalid);
    }
    if virtual_size > limit {
        return Err(ArchiveError::Budget);
    }
    Ok(virtual_size)
}

// Raw preflight bounds GNU sparse metadata before tar's logical-file reader
// allocates its block map. PAX/long-name/link/extra members are not accepted.
fn preflight(input: &mut File, limits: ArchiveLimits) -> Result<()> {
    input
        .seek(SeekFrom::Start(0))
        .map_err(|_| ArchiveError::Io)?;
    let mut magic = [0; 4];
    input
        .read_exact(&mut magic)
        .map_err(|_| ArchiveError::Invalid)?;
    if magic != [0x28, 0xb5, 0x2f, 0xfd] {
        return Err(ArchiveError::Invalid);
    }
    input
        .seek(SeekFrom::Start(0))
        .map_err(|_| ArchiveError::Io)?;
    let decoder = zstd::Decoder::new(input).map_err(|_| ArchiveError::Invalid)?;
    let mut reader = decoder.take(limits.max_expanded_bytes as u64 + TAR_OVERHEAD);
    let mut manifest = false;
    let mut disk = false;
    loop {
        let mut block = [0u8; 512];
        reader
            .read_exact(&mut block)
            .map_err(|_| ArchiveError::Invalid)?;
        if block.iter().all(|b| *b == 0) {
            let mut tail = Vec::new();
            reader
                .by_ref()
                .take(10241)
                .read_to_end(&mut tail)
                .map_err(|_| ArchiveError::Invalid)?;
            if tail.len() < 512
                || tail.len() > 10240
                || tail.iter().any(|b| *b != 0)
                || !manifest
                || !disk
            {
                return Err(ArchiveError::Invalid);
            }
            if reader.limit() == 0 {
                return Err(ArchiveError::Budget);
            }
            return Ok(());
        }
        let header = tar::Header::from_byte_slice(&block);
        let checksum: u32 = block[..148]
            .iter()
            .chain(&block[156..])
            .map(|b| *b as u32)
            .sum::<u32>()
            + 8 * 32;
        if header.cksum().map_err(|_| ArchiveError::Invalid)? != checksum
            || header.link_name_bytes().is_some()
        {
            return Err(ArchiveError::Invalid);
        }
        let size = header.entry_size().map_err(|_| ArchiveError::Invalid)?;
        let logical = header.size().map_err(|_| ArchiveError::Invalid)?;
        match header.path_bytes().as_ref() {
            b"manifest.json" if !manifest && header.entry_type().is_file() => {
                manifest = true;
                if size > MANIFEST_LIMIT {
                    return Err(ArchiveError::Budget);
                }
            }
            DISK if !disk
                && (header.entry_type().is_file() || header.entry_type().is_gnu_sparse()) =>
            {
                disk = true;
                if logical > limits.max_expanded_bytes as u64 {
                    return Err(ArchiveError::Budget);
                }
            }
            _ => return Err(ArchiveError::Invalid),
        }
        if header.entry_type().is_gnu_sparse() {
            let mut extended = header.as_gnu().ok_or(ArchiveError::Invalid)?.is_extended();
            let mut count = 0;
            while extended {
                count += 1;
                if count > SPARSE_EXTENSIONS {
                    return Err(ArchiveError::Budget);
                }
                let mut extension = tar::GnuExtSparseHeader::new();
                reader
                    .read_exact(extension.as_mut_bytes())
                    .map_err(|_| ArchiveError::Invalid)?;
                extended = extension.is_extended();
            }
        }
        if size > limits.max_expanded_bytes as u64 {
            return Err(ArchiveError::Budget);
        }
        let bytes = size.checked_add(511).ok_or(ArchiveError::Budget)? / 512 * 512;
        if io::copy(&mut reader.by_ref().take(bytes), &mut io::sink())
            .map_err(|_| ArchiveError::Invalid)?
            != bytes
        {
            return Err(ArchiveError::Invalid);
        }
    }
}

fn file_identity(file: &mut File, limit: i64) -> Result<(i64, String)> {
    let meta = file.metadata().map_err(|_| ArchiveError::Io)?;
    if !meta.is_file() || meta.len() == 0 {
        return Err(ArchiveError::Invalid);
    }
    if meta.len() > limit as u64 {
        return Err(ArchiveError::Budget);
    }
    file.seek(SeekFrom::Start(0))
        .map_err(|_| ArchiveError::Io)?;
    let mut hash = Sha256::new();
    let bytes =
        io::copy(&mut file.take(limit as u64 + 1), &mut hash).map_err(|_| ArchiveError::Io)?;
    if bytes > limit as u64 {
        return Err(ArchiveError::Budget);
    }
    if bytes != meta.len() {
        return Err(ArchiveError::Invalid);
    }
    Ok((bytes as i64, format!("{:x}", hash.finalize())))
}
struct HashReader<R> {
    input: R,
    hash: Sha256,
}
impl<R: Read> Read for HashReader<R> {
    fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
        let n = self.input.read(buffer)?;
        self.hash.update(&buffer[..n]);
        Ok(n)
    }
}
struct BoundedWriter<'a> {
    output: &'a mut File,
    remaining: u64,
    exceeded: bool,
    io_failed: bool,
}
impl Write for BoundedWriter<'_> {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        if buffer.len() as u64 > self.remaining {
            self.exceeded = true;
            return Err(io::Error::other("archive budget exhausted"));
        }
        let n = match self.output.write(buffer) {
            Ok(n) => n,
            Err(err) => {
                self.io_failed = true;
                return Err(err);
            }
        };
        self.remaining -= n as u64;
        Ok(n)
    }
    fn flush(&mut self) -> io::Result<()> {
        let result = self.output.flush();
        if result.is_err() {
            self.io_failed = true;
        }
        result
    }
}

pub(crate) fn describe_archive(
    input: &mut File,
    spec: &RestoreSpec,
    scope: &str,
    snapshot: &str,
    limits: ArchiveLimits,
) -> Result<Archive> {
    limits.validate()?;
    if !valid_id(snapshot) || !valid_scope(scope) {
        return Err(ArchiveError::Invalid);
    }
    let (size_bytes, sha256) = file_identity(input, limits.max_bytes)?;
    let scan = scan(input, spec, scope, limits, Policy::Committed)?;
    Ok(Archive {
        snapshot_id: snapshot.to_owned(),
        size_bytes,
        sha256,
        expanded_bytes: scan.expanded,
        format: ARCHIVE_FORMAT.into(),
    })
}

#[cfg(test)]
#[path = "archive_tests.rs"]
mod tests;
