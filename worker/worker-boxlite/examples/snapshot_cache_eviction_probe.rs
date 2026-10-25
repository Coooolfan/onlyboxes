//! Acceptance-only contender for a live Worker's optional archive cache.
//! The caller proves finalized shared commit and native ownership release.
//! No SDK, journal mutation, native deletion or execution authority is used.
use serde::Deserialize;
use std::{
    fs::{File, OpenOptions},
    io::{BufRead, Read, Write},
    os::unix::fs::{MetadataExt, OpenOptionsExt},
    path::{Path, PathBuf},
    time::Duration,
};
use worker_boxlite::persistence::{Archive, Transfer, TransferError};

type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    source: PathBuf,
    archive: Archive,
    wait_seconds: u64,
}

fn private_file(path: &Path, write: bool) -> Result<File> {
    let file = OpenOptions::new()
        .read(true)
        .write(write)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)?;
    let m = file.metadata()?;
    // SAFETY: geteuid reads process identity.
    if !m.is_file()
        || m.uid() != unsafe { libc::geteuid() }
        || m.mode() & 0o077 != 0
        || m.nlink() != 1
    {
        return Err("probe requires a private, owned, single-link file".into());
    }
    Ok(file)
}

fn unchanged(path: &Path, original: &std::fs::Metadata) -> Result<()> {
    let now = private_file(path, false)?.metadata()?;
    if (
        now.dev(),
        now.ino(),
        now.len(),
        now.mtime(),
        now.mtime_nsec(),
    ) != (
        original.dev(),
        original.ino(),
        original.len(),
        original.mtime(),
        original.mtime_nsec(),
    ) {
        return Err("optional source archive changed during contention".into());
    }
    Ok(())
}

fn event(name: &str) -> Result<()> {
    println!("{}", serde_json::json!({"event":name}));
    std::io::stdout().flush()?;
    Ok(())
}

async fn run() -> Result<()> {
    let stdin = std::io::stdin();
    let mut input = stdin.lock();
    let mut line = String::new();
    input.by_ref().take(65537).read_line(&mut line)?;
    if line.len() > 65536 {
        return Err("probe request too large".into());
    }
    let request: Request = serde_json::from_str(&line)?;
    request.archive.validate()?;
    if !request.source.is_absolute() || !(1..=86400).contains(&request.wait_seconds) {
        return Err("invalid probe path or wait budget".into());
    }
    let parent = request.source.parent().ok_or("missing archive directory")?;
    let m = std::fs::symlink_metadata(parent)?;
    if !m.is_dir()
        || m.mode() & 0o077 != 0
        || m.uid() != unsafe { libc::geteuid() }
        || parent.canonicalize()? != parent
    {
        return Err("probe requires a canonical private archive directory".into());
    }
    let source = private_file(&request.source, false)?;
    let original = source.metadata()?;
    if original.len() != request.archive.size_bytes as u64 {
        return Err("archive size differs".into());
    }
    let lock_path = PathBuf::from(format!("{}.lock", request.source.display()));
    let lock = private_file(&lock_path, true)?;
    if lock.metadata()?.len() != 0 {
        return Err("invalid archive lock".into());
    }
    // Check the lock domain before telling the client to start its restore.
    lock.try_lock()?;
    lock.unlock()?;
    let transfer = Transfer::new(1, Duration::from_secs(30))?;
    event("armed")?;
    let deadline = tokio::time::Instant::now() + Duration::from_secs(request.wait_seconds);
    loop {
        match lock.try_lock() {
            Ok(()) => lock.unlock()?,
            Err(std::fs::TryLockError::WouldBlock) => break,
            Err(_) => return Err("cannot observe archive lock".into()),
        }
        if tokio::time::Instant::now() >= deadline {
            return Err("did not observe the live copy lock".into());
        }
        tokio::time::sleep(Duration::from_millis(1)).await;
    }
    // This invokes the same production I/O path as Worker cache maintenance.
    // A successful deletion here fails acceptance, even if restore later works.
    if transfer
        .evict_cached_archive(request.source.clone(), &request.archive)
        .await
        != Err(TransferError::Retry)
    {
        return Err("live cache copy did not defer production eviction".into());
    }
    unchanged(&request.source, &original)?;
    event("busy_eviction_deferred")?;
    line.clear();
    input.read_line(&mut line)?;
    if line != "evict\n" {
        return Err("post-copy eviction was not requested".into());
    }
    unchanged(&request.source, &original)?;
    if transfer
        .evict_cached_archive(request.source.clone(), &request.archive)
        .await?
        != Some(original.len())
        || request.source.try_exists()?
    {
        return Err("post-copy cache eviction was not confirmed".into());
    }
    event("evicted_after_copy")?;
    Ok(())
}

#[tokio::main]
async fn main() {
    if run().await.is_err() {
        // Do not print paths, input, URLs, SDK/OS errors or private metadata.
        eprintln!("cache eviction contention probe failed; preserve fixture state");
        std::process::exit(1);
    }
}
