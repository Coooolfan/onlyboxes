//! Direct object transfers. Signed URLs and request headers are ephemeral and
//! deliberately lack Debug; public errors never include transport messages.

use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::os::unix::fs::{FileExt, MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use super::Archive;
use chrono::{DateTime, Utc};
use reqwest::{
    header::{HeaderMap, HeaderName, HeaderValue, CONTENT_LENGTH, HOST, RANGE},
    Client, Response, StatusCode,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tokio::sync::{mpsc, watch, Semaphore};
use tokio::time::Instant;
use tokio_stream::{wrappers::ReceiverStream, StreamExt};
use tokio_util::sync::CancellationToken;

const CHUNK: usize = 64 << 10;

#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum TransferError {
    #[error("snapshot transfer requires a fresh grant or retry")]
    Retry,
    #[error("snapshot transfer metadata or bytes are invalid")]
    Integrity,
    #[error("snapshot private transfer file is unavailable")]
    Local,
}
type Result<T> = std::result::Result<T, TransferError>;

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TransferGrant {
    pub url: String,
    pub method: String,
    #[serde(default, deserialize_with = "nullable_headers")]
    pub headers: BTreeMap<String, Vec<String>>,
    pub expires_at: DateTime<Utc>,
    pub offset: i64,
    pub size_bytes: i64,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct UploadPart {
    pub part_number: i32,
    pub grant: TransferGrant,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CompletedPart {
    pub part_number: i32,
    pub etag: String,
    pub size_bytes: i64,
}

fn nullable_headers<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> std::result::Result<BTreeMap<String, Vec<String>>, D::Error> {
    Ok(Option::<BTreeMap<String, Vec<String>>>::deserialize(deserializer)?.unwrap_or_default())
}

#[derive(Clone, PartialEq, Eq)]
struct Identity {
    dev: u64,
    ino: u64,
    len: u64,
    mtime: i64,
    mtime_ns: i64,
    ctime: i64,
    ctime_ns: i64,
}
fn identity(file: &File) -> Result<Identity> {
    let m = file.metadata().map_err(|_| TransferError::Local)?;
    if !m.is_file() {
        return Err(TransferError::Integrity);
    }
    Ok(Identity {
        dev: m.dev(),
        ino: m.ino(),
        len: m.len(),
        mtime: m.mtime(),
        mtime_ns: m.mtime_nsec(),
        ctime: m.ctime(),
        ctime_ns: m.ctime_nsec(),
    })
}

pub struct VerifiedSource {
    file: Arc<File>,
    archive: Archive,
    identity: Identity,
}
pub struct Downloaded {
    pub file: File,
    pub reused: bool,
    pub resumed_from: i64,
}

#[derive(Clone)]
pub struct Transfer {
    client: Client,
    slots: Arc<Semaphore>,
    io_slots: Arc<Semaphore>,
    idle: Duration,
}
impl Transfer {
    pub fn new(concurrency: usize, idle: Duration) -> Result<Self> {
        if !(1..=16).contains(&concurrency)
            || idle < Duration::from_secs(1)
            || idle > Duration::from_secs(600)
        {
            return Err(TransferError::Integrity);
        }
        let client = Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .no_gzip()
            .no_brotli()
            .no_deflate()
            .no_zstd()
            .connect_timeout(idle)
            .pool_max_idle_per_host(concurrency)
            .build()
            .map_err(|_| TransferError::Retry)?;
        Ok(Self {
            client,
            slots: Arc::new(Semaphore::new(concurrency)),
            io_slots: Arc::new(Semaphore::new(concurrency)),
            idle,
        })
    }

    /// Verify once per immutable, pinned source rather than rehashing the
    /// complete archive for every batch of part grants.
    pub async fn source(
        &self,
        file: File,
        archive: Archive,
        cancel: CancellationToken,
    ) -> Result<VerifiedSource> {
        archive.validate().map_err(|_| TransferError::Integrity)?;
        let child = cancel.child_token();
        let _guard = CancelOnDrop(child.clone());
        let permit = tokio::select! {
            biased;
            _ = child.cancelled() => return Err(TransferError::Retry),
            p = self.io_slots.clone().acquire_owned() => p.map_err(|_|TransferError::Retry)?,
        };
        let file = Arc::new(file);
        let captured = file.clone();
        let expected = archive.clone();
        let token = child.clone();
        let work = tokio::task::spawn_blocking(move || {
            let _permit = permit;
            verify_file(&captured, &expected, &token)
        });
        let identity = tokio::select! {
            biased;
            _ = child.cancelled() => return Err(TransferError::Retry),
            result = work => result.map_err(|_|TransferError::Local)??,
        };
        Ok(VerifiedSource {
            file,
            archive,
            identity,
        })
    }

    pub async fn upload(
        &self,
        source: &VerifiedSource,
        parts: &[UploadPart],
        cancel: CancellationToken,
    ) -> Result<Vec<CompletedPart>> {
        if parts.is_empty() || parts.len() > 16 || identity(&source.file)? != source.identity {
            return Err(TransferError::Integrity);
        }
        let mut seen = BTreeSet::new();
        let mut ranges = Vec::new();
        for part in parts {
            if !(1..=10000).contains(&part.part_number)
                || !seen.insert(part.part_number)
                || part.grant.offset < 0
                || part.grant.size_bytes <= 0
                || part.grant.offset > source.archive.size_bytes
                || part.grant.size_bytes > source.archive.size_bytes - part.grant.offset
            {
                return Err(TransferError::Integrity);
            }
            checked_grant(&part.grant, "PUT")?;
            ranges.push((part.grant.offset, part.grant.offset + part.grant.size_bytes));
        }
        ranges.sort_unstable();
        if ranges.windows(2).any(|pair| pair[0].1 > pair[1].0) {
            return Err(TransferError::Integrity);
        }
        let child = cancel.child_token();
        let _guard = CancelOnDrop(child.clone());
        let mut jobs = tokio::task::JoinSet::new();
        for part in parts.iter().cloned() {
            let transfer = self.clone();
            let file = source.file.clone();
            let token = child.clone();
            jobs.spawn(async move { transfer.upload_one(file, part, token).await });
        }
        let mut completed = Vec::new();
        let mut error = None;
        while let Some(result) = jobs.join_next().await {
            match result {
                Ok(Ok(part)) => completed.push(part),
                Ok(Err(err)) => {
                    error.get_or_insert(err);
                    child.cancel();
                }
                Err(_) => {
                    error.get_or_insert(TransferError::Retry);
                    child.cancel();
                }
            }
        }
        if let Some(error) = error {
            return Err(error);
        }
        if identity(&source.file)? != source.identity {
            return Err(TransferError::Integrity);
        }
        completed.sort_by_key(|part| part.part_number);
        Ok(completed)
    }

    async fn upload_one(
        &self,
        file: Arc<File>,
        part: UploadPart,
        cancel: CancellationToken,
    ) -> Result<CompletedPart> {
        let grant = part.grant.clone();
        self.exchange(&grant, &cancel, |progress, token| async move {
            let (tx, rx) = mpsc::channel::<std::io::Result<Vec<u8>>>(1);
            let mut offset = part.grant.offset as u64;
            let end = offset + part.grant.size_bytes as u64;
            let pump_token = token.clone();
            let transfer = self.clone();
            tokio::spawn(async move {
                while offset < end {
                    if pump_token.is_cancelled() || tx.is_closed() {
                        return;
                    }
                    let length = ((end - offset) as usize).min(CHUNK);
                    let source = file.clone();
                    let cancel = pump_token.clone();
                    let read = transfer
                        .io_task(move || {
                            if cancel.is_cancelled() {
                                return Err(TransferError::Retry);
                            }
                            let mut data = vec![0; length];
                            let n = source
                                .read_at(&mut data, offset)
                                .map_err(|_| TransferError::Local)?;
                            data.truncate(n);
                            Ok(data)
                        })
                        .await;
                    let data = match read {
                        Ok(data) if !data.is_empty() => data,
                        _ => {
                            let _ = tx
                                .send(Err(std::io::Error::other("snapshot source read failed")))
                                .await;
                            return;
                        }
                    };
                    offset += data.len() as u64;
                    tokio::select! {
                        _ = pump_token.cancelled() => return,
                        result = tx.send(Ok(data)) => if result.is_err() { return; }
                    }
                }
            });
            let stream = ReceiverStream::new(rx).map(move |chunk| {
                let _ = progress.send(Instant::now());
                chunk
            });
            let headers = checked_grant(&part.grant, "PUT")?;
            let response = self
                .client
                .put(&part.grant.url)
                .headers(headers)
                .header(CONTENT_LENGTH, part.grant.size_bytes)
                .body(reqwest::Body::wrap_stream(stream))
                .send()
                .await
                .map_err(|_| TransferError::Retry)?;
            if response.status() != StatusCode::OK {
                return Err(TransferError::Retry);
            }
            let etag = response
                .headers()
                .get("etag")
                .and_then(|v| v.to_str().ok())
                .filter(|s| !s.is_empty() && s.len() <= 1024)
                .ok_or(TransferError::Integrity)?
                .to_owned();
            Ok(CompletedPart {
                part_number: part.part_number,
                etag,
                size_bytes: part.grant.size_bytes,
            })
        })
        .await
    }

    // A dropped Future cannot stop an already running filesystem call. Keep its
    // permit in the blocking task until that call actually finishes.
    async fn io_task<T: Send + 'static>(
        &self,
        work: impl FnOnce() -> Result<T> + Send + 'static,
    ) -> Result<T> {
        let permit = self
            .io_slots
            .clone()
            .acquire_owned()
            .await
            .map_err(|_| TransferError::Retry)?;
        tokio::task::spawn_blocking(move || {
            let _permit = permit;
            work()
        })
        .await
        .map_err(|_| TransferError::Local)?
    }

    pub async fn verify_remote(
        &self,
        grant: &TransferGrant,
        archive: &Archive,
        cancel: CancellationToken,
    ) -> Result<()> {
        full_grant(grant, archive)?;
        self.exchange(grant, &cancel, |progress, _| async move {
            let mut response = self
                .client
                .get(&grant.url)
                .headers(checked_grant(grant, "GET")?)
                .send()
                .await
                .map_err(|_| TransferError::Retry)?;
            if response.status() != StatusCode::OK {
                return Err(TransferError::Retry);
            }
            check_encoding(&response)?;
            if response.headers().contains_key("content-range")
                || response
                    .content_length()
                    .is_some_and(|n| n != archive.size_bytes as u64)
            {
                return Err(TransferError::Integrity);
            }
            let mut hash = Sha256::new();
            let mut total = 0i64;
            while let Some(chunk) = response.chunk().await.map_err(|_| TransferError::Retry)? {
                total = total
                    .checked_add(chunk.len() as i64)
                    .ok_or(TransferError::Integrity)?;
                if total > archive.size_bytes {
                    return Err(TransferError::Integrity);
                }
                hash.update(&chunk);
                let _ = progress.send(Instant::now());
            }
            if total != archive.size_bytes || format!("{:x}", hash.finalize()) != archive.sha256 {
                return Err(TransferError::Integrity);
            }
            Ok(())
        })
        .await
    }

    async fn exchange<T, F, Fut>(
        &self,
        grant: &TransferGrant,
        cancel: &CancellationToken,
        action: F,
    ) -> Result<T>
    where
        F: FnOnce(watch::Sender<Instant>, CancellationToken) -> Fut,
        Fut: std::future::Future<Output = Result<T>>,
    {
        let remaining = grant
            .expires_at
            .signed_duration_since(Utc::now())
            .to_std()
            .map_err(|_| TransferError::Retry)?;
        if remaining.is_zero() || remaining > Duration::from_secs(86400) {
            return Err(TransferError::Retry);
        }
        let deadline = Instant::now() + remaining;
        let token = cancel.child_token();
        let _guard = CancelOnDrop(token.clone());
        let _permit = tokio::select! {
            biased;
            _ = token.cancelled() => return Err(TransferError::Retry),
            _ = tokio::time::sleep_until(deadline) => return Err(TransferError::Retry),
            p = self.slots.clone().acquire_owned() => p.map_err(|_|TransferError::Retry)?,
        };
        let (tx, mut rx) = watch::channel(Instant::now());
        let _keepalive = tx.clone();
        let idle = self.idle;
        let idle_wait = async move {
            loop {
                let until = *rx.borrow() + idle;
                tokio::select! {
                    _ = tokio::time::sleep_until(until) => {
                        if Instant::now() >= *rx.borrow() + idle { return; }
                    },
                    result = rx.changed() => if result.is_err() { return; },
                }
            }
        };
        if token.is_cancelled() || Instant::now() >= deadline {
            return Err(TransferError::Retry);
        }
        tokio::select! {
            biased;
            _ = token.cancelled() => Err(TransferError::Retry),
            _ = tokio::time::sleep_until(deadline) => Err(TransferError::Retry),
            _ = idle_wait => Err(TransferError::Retry),
            result = action(tx, token.clone()) => result,
        }
    }
}

fn checked_grant(grant: &TransferGrant, method: &str) -> Result<HeaderMap> {
    if grant.method != method
        || grant.size_bytes <= 0
        || grant.offset < 0
        || grant.url.len() > 16 << 10
        || grant.url.trim() != grant.url
    {
        return Err(TransferError::Integrity);
    }
    let url = reqwest::Url::parse(&grant.url).map_err(|_| TransferError::Integrity)?;
    let authority = grant
        .url
        .split_once("://")
        .and_then(|(_, s)| s.split(['/', '?', '#']).next())
        .ok_or(TransferError::Integrity)?;
    if !matches!(url.scheme(), "http" | "https")
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.fragment().is_some()
        || authority.contains('@')
        || url.port() == Some(0)
    {
        return Err(TransferError::Integrity);
    }
    let mut headers = HeaderMap::new();
    let mut size = 0;
    if grant.headers.len() > 64 {
        return Err(TransferError::Integrity);
    }
    for (key, values) in &grant.headers {
        if values.len() > 16 {
            return Err(TransferError::Integrity);
        }
        let name = HeaderName::from_bytes(key.as_bytes()).map_err(|_| TransferError::Integrity)?;
        if matches!(
            name.as_str(),
            "connection"
                | "proxy-authorization"
                | "transfer-encoding"
                | "trailer"
                | "upgrade"
                | "te"
                | "range"
        ) {
            return Err(TransferError::Integrity);
        }
        for value in values {
            size += key.len() + value.len();
            if size > 32 << 10 {
                return Err(TransferError::Integrity);
            }
            if name == HOST && value != authority {
                return Err(TransferError::Integrity);
            }
            if name == CONTENT_LENGTH
                && value.parse::<i64>().ok()
                    != Some(if method == "PUT" { grant.size_bytes } else { 0 })
            {
                return Err(TransferError::Integrity);
            }
            if name == "accept-encoding" && value != "identity" {
                return Err(TransferError::Integrity);
            }
            headers.append(
                name.clone(),
                HeaderValue::from_str(value).map_err(|_| TransferError::Integrity)?,
            );
        }
    }
    if headers.get_all(HOST).iter().count() > 1
        || headers.get_all(CONTENT_LENGTH).iter().count() > 1
    {
        return Err(TransferError::Integrity);
    }
    headers.insert(
        HOST,
        HeaderValue::from_str(authority).map_err(|_| TransferError::Integrity)?,
    );
    Ok(headers)
}
fn full_grant(grant: &TransferGrant, archive: &Archive) -> Result<()> {
    archive.validate().map_err(|_| TransferError::Integrity)?;
    if grant.offset != 0 || grant.size_bytes != archive.size_bytes {
        return Err(TransferError::Integrity);
    }
    checked_grant(grant, "GET")?;
    Ok(())
}
fn check_encoding(response: &Response) -> Result<()> {
    if response
        .headers()
        .get("content-encoding")
        .is_some_and(|value| value != "identity")
    {
        return Err(TransferError::Integrity);
    }
    Ok(())
}
fn verify_file(file: &File, archive: &Archive, cancel: &CancellationToken) -> Result<Identity> {
    let before = identity(file)?;
    if before.len != archive.size_bytes as u64 {
        return Err(TransferError::Integrity);
    }
    let mut hash = Sha256::new();
    let mut offset = 0;
    let mut data = vec![0; CHUNK];
    while offset < before.len {
        if cancel.is_cancelled() {
            return Err(TransferError::Retry);
        }
        let n = file
            .read_at(
                &mut data[..((before.len - offset) as usize).min(CHUNK)],
                offset,
            )
            .map_err(|_| TransferError::Local)?;
        if n == 0 {
            return Err(TransferError::Integrity);
        }
        hash.update(&data[..n]);
        offset += n as u64;
    }
    if format!("{:x}", hash.finalize()) != archive.sha256 || identity(file)? != before {
        return Err(TransferError::Integrity);
    }
    Ok(before)
}
struct CancelOnDrop(CancellationToken);
impl Drop for CancelOnDrop {
    fn drop(&mut self) {
        self.0.cancel();
    }
}

#[path = "transfer_download.rs"]
mod download;
