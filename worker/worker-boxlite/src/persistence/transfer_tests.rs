use super as download;
use super::*;
use std::os::unix::fs::{symlink, PermissionsExt};
use std::sync::{
    atomic::{AtomicUsize, Ordering},
    Mutex,
};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        let p = std::env::temp_dir().join(format!("onlyboxes-transfer-{}", uuid::Uuid::new_v4()));
        fs::create_dir(&p).unwrap();
        fs::set_permissions(&p, fs::Permissions::from_mode(0o700)).unwrap();
        Self(p)
    }
    fn destination(&self) -> PathBuf {
        self.0.join("snapshot.boxlite")
    }
    fn source(&self, data: &[u8]) -> File {
        let path = self.0.join("source");
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&path)
            .unwrap();
        file.write_all(data).unwrap();
        drop(file);
        File::open(path).unwrap()
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
fn archive(data: &[u8]) -> Archive {
    Archive {
        snapshot_id: "snapshot-1".into(),
        size_bytes: data.len() as i64,
        expanded_bytes: 1 << 20,
        sha256: format!("{:x}", Sha256::digest(data)),
        format: crate::persistence::ARCHIVE_FORMAT.into(),
    }
}
struct Request {
    method: String,
    path: String,
    headers: BTreeMap<String, String>,
    body: Vec<u8>,
}
struct Reply {
    status: u16,
    headers: Vec<(String, String)>,
    body: Vec<u8>,
    length: Option<usize>,
    truncate: Option<usize>,
    delay: Duration,
    body_pause: Option<Duration>,
}
impl Reply {
    fn bytes(data: &[u8]) -> Self {
        Self {
            status: 200,
            headers: Vec::new(),
            body: data.to_vec(),
            length: Some(data.len()),
            truncate: None,
            delay: Duration::ZERO,
            body_pause: None,
        }
    }
}
struct Server {
    url: String,
    hits: Arc<AtomicUsize>,
    max: Arc<AtomicUsize>,
    task: tokio::task::JoinHandle<()>,
}
impl Drop for Server {
    fn drop(&mut self) {
        self.task.abort();
    }
}
impl Server {
    async fn new(handler: impl Fn(Request) -> Reply + Send + Sync + 'static) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let handler = Arc::new(handler);
        let hits = Arc::new(AtomicUsize::new(0));
        let active = Arc::new(AtomicUsize::new(0));
        let max = Arc::new(AtomicUsize::new(0));
        let count = hits.clone();
        let maximum = max.clone();
        let task = tokio::spawn(async move {
            loop {
                let Ok((socket, _)) = listener.accept().await else {
                    return;
                };
                let handler = handler.clone();
                let count = count.clone();
                let active = active.clone();
                let max = maximum.clone();
                tokio::spawn(async move {
                    if let Ok((mut socket, request)) = read_request(socket).await {
                        count.fetch_add(1, Ordering::SeqCst);
                        let n = active.fetch_add(1, Ordering::SeqCst) + 1;
                        max.fetch_max(n, Ordering::SeqCst);
                        let reply = handler(request);
                        tokio::time::sleep(reply.delay).await;
                        active.fetch_sub(1, Ordering::SeqCst);
                        let mut header =
                            format!("HTTP/1.1 {} result\r\nConnection: close\r\n", reply.status);
                        if let Some(length) = reply.length {
                            header.push_str(&format!("Content-Length: {length}\r\n"));
                        }
                        for (k, v) in reply.headers {
                            header.push_str(&format!("{k}: {v}\r\n"));
                        }
                        header.push_str("\r\n");
                        if socket.write_all(header.as_bytes()).await.is_err() {
                            return;
                        }
                        let length = reply
                            .truncate
                            .unwrap_or(reply.body.len())
                            .min(reply.body.len());
                        if let Some(pause) = reply.body_pause {
                            let prefix = length.min(4096);
                            let _ = socket.write_all(&reply.body[..prefix]).await;
                            let _ = socket.flush().await;
                            tokio::time::sleep(pause).await;
                            let _ = socket.write_all(&reply.body[prefix..length]).await;
                        } else {
                            let _ = socket.write_all(&reply.body[..length]).await;
                        }
                        let _ = socket.flush().await;
                        if reply.truncate.is_some() {
                            tokio::time::sleep(Duration::from_millis(75)).await;
                        }
                        let _ = socket.shutdown().await;
                    }
                });
            }
        });
        Self {
            url: format!("http://{address}/object?grant=private"),
            hits,
            max,
            task,
        }
    }
    fn grant(&self, method: &str, size: usize) -> TransferGrant {
        TransferGrant {
            url: self.url.clone(),
            method: method.into(),
            headers: BTreeMap::new(),
            expires_at: Utc::now() + chrono::Duration::minutes(2),
            offset: 0,
            size_bytes: size as i64,
        }
    }
}
async fn read_request(mut socket: TcpStream) -> std::io::Result<(TcpStream, Request)> {
    let mut data = Vec::new();
    let end = loop {
        let mut buf = [0; 4096];
        let n = socket.read(&mut buf).await?;
        if n == 0 {
            return Err(std::io::ErrorKind::UnexpectedEof.into());
        }
        data.extend(&buf[..n]);
        if let Some(i) = data.windows(4).position(|s| s == b"\r\n\r\n") {
            break i + 4;
        }
        if data.len() > 64 << 10 {
            return Err(std::io::Error::other("headers too large"));
        }
    };
    let text = String::from_utf8_lossy(&data[..end]);
    let mut lines = text.split("\r\n");
    let mut first = lines.next().unwrap().split_whitespace();
    let method = first.next().unwrap().to_owned();
    let path = first.next().unwrap().to_owned();
    let mut headers = BTreeMap::new();
    for line in lines {
        if let Some((k, v)) = line.split_once(':') {
            headers.insert(k.to_ascii_lowercase(), v.trim().to_owned());
        }
    }
    let length = headers
        .get("content-length")
        .and_then(|s| s.parse::<usize>().ok())
        .unwrap_or(0);
    let mut body = data[end..].to_vec();
    while body.len() < length {
        let mut buf = vec![0; (length - body.len()).min(65536)];
        let n = socket.read(&mut buf).await?;
        if n == 0 {
            return Err(std::io::ErrorKind::UnexpectedEof.into());
        }
        body.extend(&buf[..n]);
    }
    Ok((
        socket,
        Request {
            method,
            path,
            headers,
            body,
        },
    ))
}
fn transfer() -> Transfer {
    Transfer::new(2, Duration::from_secs(1)).unwrap()
}

#[tokio::test]
async fn multipart_upload_uses_independent_offsets_bounded_connections_and_delayed_ack() {
    let data: Vec<_> = (0..3 * CHUNK).map(|i| (i % 251) as u8).collect();
    let root = Root::new();
    let received = Arc::new(Mutex::new(BTreeMap::new()));
    let captured = received.clone();
    let server = Server::new(move |request| {
        assert_eq!(request.method, "PUT");
        let number: usize = request.path.split("part=").nth(1).unwrap().parse().unwrap();
        captured.lock().unwrap().insert(number, request.body);
        let mut reply = Reply::bytes(&[]);
        reply
            .headers
            .push(("ETag".into(), format!("\"part-{number}\"")));
        reply.delay = Duration::from_millis(75);
        reply
    })
    .await;
    let t = transfer();
    let source = t
        .source(root.source(&data), archive(&data), CancellationToken::new())
        .await
        .unwrap();
    let parts: Vec<_> = (0..3)
        .map(|i| {
            let mut grant = server.grant("PUT", CHUNK);
            grant.url = format!("{}&part={}", server.url, i + 1);
            grant.offset = (i * CHUNK) as i64;
            UploadPart {
                part_number: (i + 1) as i32,
                grant,
            }
        })
        .collect();
    let done = t
        .upload(&source, &parts, CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(done.len(), 3);
    assert!(server.max.load(Ordering::SeqCst) <= 2);
    for i in 0..3 {
        assert!(received.lock().unwrap()[&(i + 1)] == data[i * CHUNK..(i + 1) * CHUNK]);
        assert_eq!(done[i].size_bytes, CHUNK as i64);
    }
}

#[tokio::test]
async fn invalid_parts_and_changed_source_never_start_requests() {
    let root = Root::new();
    let data = b"source-data";
    let server = Server::new(|_| panic!("invalid upload reached network")).await;
    let t = transfer();
    let source = t
        .source(root.source(data), archive(data), CancellationToken::new())
        .await
        .unwrap();
    let part = UploadPart {
        part_number: 1,
        grant: server.grant("PUT", data.len()),
    };
    assert_eq!(
        t.upload(
            &source,
            &[part.clone(), part.clone()],
            CancellationToken::new()
        )
        .await
        .err(),
        Some(TransferError::Integrity)
    );
    let mut overlapping = part.clone();
    overlapping.part_number = 2;
    assert_eq!(
        t.upload(
            &source,
            &[part.clone(), overlapping],
            CancellationToken::new()
        )
        .await
        .err(),
        Some(TransferError::Integrity)
    );
    OpenOptions::new()
        .write(true)
        .open(root.0.join("source"))
        .unwrap()
        .write_all(b"changed")
        .unwrap();
    assert_eq!(
        t.upload(&source, &[part], CancellationToken::new())
            .await
            .err(),
        Some(TransferError::Integrity)
    );
    assert_eq!(server.hits.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn remote_readback_checks_complete_size_digest_and_encoding() {
    let data = b"verified-content";
    for mode in ["good", "corrupt", "large", "encoded"] {
        let server = Server::new(move |_| {
            let mut reply = Reply::bytes(data);
            match mode {
                "corrupt" => reply.body[0] ^= 1,
                "large" => {
                    reply.body.extend(b"more");
                    reply.length = None;
                }
                "encoded" => reply
                    .headers
                    .push(("Content-Encoding".into(), "gzip".into())),
                _ => {}
            }
            reply
        })
        .await;
        let result = transfer()
            .verify_remote(
                &server.grant("GET", data.len()),
                &archive(data),
                CancellationToken::new(),
            )
            .await;
        if mode == "good" {
            assert_eq!(result, Ok(()));
        } else {
            assert_eq!(result, Err(TransferError::Integrity), "mode {mode}");
        }
    }
}

#[tokio::test]
async fn partial_download_resumes_then_reuses_verified_cache_without_a_live_grant() {
    let data: Vec<_> = (0..2 * CHUNK).map(|i| (i % 239) as u8).collect();
    let served = data.clone();
    let calls = Arc::new(AtomicUsize::new(0));
    let count = calls.clone();
    let server = Server::new(move |request| {
        let call = count.fetch_add(1, Ordering::SeqCst);
        if call == 0 {
            let mut reply = Reply::bytes(&served);
            reply.truncate = Some(4096);
            reply
        } else {
            let range = request.headers.get("range").unwrap();
            let offset = range
                .trim_start_matches("bytes=")
                .trim_end_matches('-')
                .parse::<usize>()
                .unwrap();
            let mut reply = Reply::bytes(&served[offset..]);
            reply.status = 206;
            reply.headers.push((
                "Content-Range".into(),
                format!("bytes {}-{}/{}", offset, served.len() - 1, served.len()),
            ));
            reply
        }
    })
    .await;
    let root = Root::new();
    let t = transfer();
    let grant = server.grant("GET", data.len());
    let descriptor = archive(&data);
    assert_eq!(
        t.download(
            &grant,
            &descriptor,
            root.destination(),
            CancellationToken::new()
        )
        .await
        .err(),
        Some(TransferError::Retry)
    );
    assert!(!root.destination().exists());
    let partial = root.0.join("snapshot.boxlite.partial");
    let prefix = fs::metadata(&partial).unwrap().len();
    assert!(prefix > 0 && prefix < data.len() as u64);
    let result = t
        .download(
            &grant,
            &descriptor,
            root.destination(),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(result.resumed_from, prefix as i64);
    assert!(!result.reused);
    assert!(fs::read(root.destination()).unwrap() == data);
    assert_eq!(result.file.metadata().unwrap().mode() & 0o777, 0o600);
    let mut expired = grant;
    expired.expires_at = Utc::now() - chrono::Duration::seconds(1);
    let cached = t
        .download(
            &expired,
            &descriptor,
            root.destination(),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(cached.reused);
    assert_eq!(server.hits.load(Ordering::SeqCst), 2);
    assert!(!partial.exists());
}

#[tokio::test]
async fn a_provider_ignoring_range_restarts_from_zero() {
    let data = b"complete-object";
    let server = Server::new(|request| {
        assert_eq!(
            request.headers.get("range").map(String::as_str),
            Some("bytes=3-")
        );
        Reply::bytes(data)
    })
    .await;
    let root = Root::new();
    let descriptor = archive(data);
    let local = download::prepare(root.destination(), &descriptor).unwrap();
    local.file.write_all_at(b"bad", 0).unwrap();
    drop(local);
    let result = transfer()
        .download(
            &server.grant("GET", data.len()),
            &descriptor,
            root.destination(),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(result.resumed_from, 0);
    assert!(fs::read(root.destination()).unwrap() == data);
}

#[tokio::test]
async fn redirects_are_not_followed() {
    let other = Server::new(|_| Reply::bytes(b"secret")).await;
    let location = other.url.clone();
    let server = Server::new(move |_| {
        let mut reply = Reply::bytes(&[]);
        reply.status = 302;
        reply.headers.push(("Location".into(), location.clone()));
        reply
    })
    .await;
    assert_eq!(
        transfer()
            .verify_remote(
                &server.grant("GET", 6),
                &archive(b"secret"),
                CancellationToken::new()
            )
            .await,
        Err(TransferError::Retry)
    );
    assert_eq!(other.hits.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn expired_and_cancelled_grants_waiting_for_capacity_send_nothing() {
    let server = Server::new(|_| panic!("expired request reached network")).await;
    let t = transfer();
    let _a = t.slots.clone().acquire_owned().await.unwrap();
    let _b = t.slots.clone().acquire_owned().await.unwrap();
    let mut grant = server.grant("GET", 4);
    grant.expires_at = Utc::now() + chrono::Duration::milliseconds(30);
    assert_eq!(
        t.verify_remote(&grant, &archive(b"data"), CancellationToken::new())
            .await,
        Err(TransferError::Retry)
    );
    let cancel = CancellationToken::new();
    cancel.cancel();
    assert_eq!(
        t.verify_remote(&server.grant("GET", 4), &archive(b"data"), cancel)
            .await,
        Err(TransferError::Retry)
    );
    assert_eq!(server.hits.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn malformed_headers_urls_and_go_null_headers_are_checked() {
    let server = Server::new(|_| panic!("invalid grant reached network")).await;
    let grant = server.grant("GET", 4);
    let mut value = serde_json::to_value(&grant).unwrap();
    value["headers"] = serde_json::Value::Null;
    let parsed: TransferGrant = serde_json::from_value(value).unwrap();
    assert!(parsed.headers.is_empty());
    let changes: Vec<fn(&mut TransferGrant)> = vec![
        |g| g.url = "file:///private".into(),
        |g| g.url = "https://user:password@invalid/object".into(),
        |g| g.url.push_str("#fragment"),
        |g| {
            g.headers.insert("Host".into(), vec!["other-host".into()]);
        },
        |g| {
            g.headers
                .insert("x-amz-test".into(), vec!["inject\r\nHost: other".into()]);
        },
        |g| {
            g.headers.insert("Range".into(), vec!["bytes=0-".into()]);
        },
    ];
    for change in changes {
        let mut bad = grant.clone();
        change(&mut bad);
        assert_eq!(
            transfer()
                .verify_remote(&bad, &archive(b"data"), CancellationToken::new())
                .await,
            Err(TransferError::Integrity)
        );
    }
    assert_eq!(server.hits.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn private_files_are_not_redirected_or_rebound_to_another_snapshot() {
    let root = Root::new();
    let descriptor = archive(b"data");
    let local = download::prepare(root.destination(), &descriptor).unwrap();
    local.file.write_all_at(b"da", 0).unwrap();
    drop(local);
    let mut other = descriptor.clone();
    other.snapshot_id = "different".into();
    assert!(matches!(
        download::prepare(root.destination(), &other),
        Err(TransferError::Integrity)
    ));
    fs::remove_file(root.0.join("snapshot.boxlite.partial")).unwrap();
    let outside = root.0.join("outside");
    fs::write(&outside, b"unchanged").unwrap();
    symlink(&outside, root.0.join("snapshot.boxlite.partial")).unwrap();
    assert!(download::prepare(root.destination(), &descriptor).is_err());
    assert_eq!(fs::read(outside).unwrap(), b"unchanged");
}

#[tokio::test]
async fn publication_ack_loss_recovers_the_same_inode_without_network() {
    let root = Root::new();
    let data = b"cached-object";
    let descriptor = archive(data);
    let local = download::prepare(root.destination(), &descriptor).unwrap();
    local.file.write_all_at(data, 0).unwrap();
    fs::hard_link(&local.partial, &local.destination).unwrap();
    drop(local);
    let server = Server::new(|_| panic!("complete local archive fetched again")).await;
    let done = transfer()
        .download(
            &server.grant("GET", data.len()),
            &descriptor,
            root.destination(),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(done.reused);
    assert_eq!(done.file.metadata().unwrap().nlink(), 1);
    assert!(!root.0.join("snapshot.boxlite.partial").exists());
    assert_eq!(server.hits.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn one_writer_holds_the_partial_file_lock() {
    let root = Root::new();
    let descriptor = archive(b"data");
    let first = download::prepare(root.destination(), &descriptor).unwrap();
    assert!(matches!(
        download::prepare(root.destination(), &descriptor),
        Err(TransferError::Retry)
    ));
    drop(first);
    assert!(download::prepare(root.destination(), &descriptor).is_ok());
}

#[tokio::test]
async fn stalled_headers_body_and_expiring_grants_release_capacity() {
    for mode in ["headers", "body", "expiry"] {
        let server = Server::new(move |_| {
            let mut reply = Reply::bytes(&[b'x'; 8192]);
            if mode == "body" {
                reply.body_pause = Some(Duration::from_secs(3));
            } else {
                reply.delay = Duration::from_secs(3);
            }
            reply
        })
        .await;
        let t = Transfer::new(1, Duration::from_secs(1)).unwrap();
        let mut grant = server.grant("GET", 8192);
        if mode == "expiry" {
            grant.expires_at = Utc::now() + chrono::Duration::milliseconds(100);
        }
        let result = tokio::time::timeout(
            Duration::from_secs(2),
            t.verify_remote(&grant, &archive(&[b'x'; 8192]), CancellationToken::new()),
        )
        .await
        .unwrap();
        assert_eq!(result, Err(TransferError::Retry), "{mode}");
        assert_eq!(t.slots.available_permits(), 1);
    }
}

#[tokio::test]
async fn cancellation_preserves_only_a_private_prefix_then_resumes() {
    let data = vec![b'z'; 2 * CHUNK];
    let served = data.clone();
    let server = Server::new(move |request| {
        let offset = request
            .headers
            .get("range")
            .and_then(|s| s.strip_prefix("bytes="))
            .and_then(|s| s.strip_suffix('-'))
            .map(|s| s.parse::<usize>().unwrap())
            .unwrap_or(0);
        let mut reply = Reply::bytes(&served[offset..]);
        if offset == 0 {
            reply.body_pause = Some(Duration::from_secs(3));
        } else {
            reply.status = 206;
            reply.headers.push((
                "Content-Range".into(),
                format!("bytes {}-{}/{}", offset, served.len() - 1, served.len()),
            ));
        }
        reply
    })
    .await;
    let root = Root::new();
    let t = transfer();
    let grant = server.grant("GET", data.len());
    let descriptor = archive(&data);
    let cancel = CancellationToken::new();
    let captured = cancel.clone();
    let transfer = t.clone();
    let request = grant.clone();
    let expected = descriptor.clone();
    let destination = root.destination();
    let work = tokio::spawn(async move {
        transfer
            .download(&request, &expected, destination, captured)
            .await
    });
    let partial = root.0.join("snapshot.boxlite.partial");
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            if fs::metadata(&partial).is_ok_and(|m| m.len() > 0) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
    cancel.cancel();
    assert_eq!(work.await.unwrap().err(), Some(TransferError::Retry));
    assert!(!root.destination().exists());
    let prefix = fs::metadata(&partial).unwrap().len();
    assert!(prefix > 0 && prefix < data.len() as u64);
    let done = t
        .download(
            &grant,
            &descriptor,
            root.destination(),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(done.resumed_from, prefix as i64);
    assert_eq!(fs::read(root.destination()).unwrap(), data);
}

#[tokio::test]
async fn invalid_ranges_or_digest_discard_partial_without_publication() {
    for mode in ["range", "digest"] {
        let data = b"verified-object";
        let server = Server::new(move |_| {
            let mut reply = Reply::bytes(&data[3..]);
            reply.status = 206;
            reply.headers.push((
                "Content-Range".into(),
                if mode == "range" {
                    format!("bytes 2-{}/{}", data.len() - 1, data.len())
                } else {
                    format!("bytes 3-{}/{}", data.len() - 1, data.len())
                },
            ));
            reply
        })
        .await;
        let root = Root::new();
        let descriptor = archive(data);
        let local = download::prepare(root.destination(), &descriptor).unwrap();
        local.file.write_all_at(b"bad", 0).unwrap();
        drop(local);
        assert_eq!(
            transfer()
                .download(
                    &server.grant("GET", data.len()),
                    &descriptor,
                    root.destination(),
                    CancellationToken::new()
                )
                .await
                .err(),
            Some(TransferError::Integrity)
        );
        assert!(!root.destination().exists());
        assert!(!root.0.join("snapshot.boxlite.partial").exists());
        assert!(!root.0.join("snapshot.boxlite.partial.json").exists());
    }
}

#[tokio::test]
async fn source_hash_mismatch_and_missing_upload_receipt_fail_closed() {
    let root = Root::new();
    let data = b"immutable-archive";
    let t = transfer();
    assert_eq!(
        t.source(
            root.source(data),
            archive(b"different-content"),
            CancellationToken::new()
        )
        .await
        .err(),
        Some(TransferError::Integrity)
    );
    let server = Server::new(|_| Reply::bytes(&[])).await;
    let source = t
        .source(
            File::open(root.0.join("source")).unwrap(),
            archive(data),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(
        t.upload(
            &source,
            &[UploadPart {
                part_number: 1,
                grant: server.grant("PUT", data.len())
            }],
            CancellationToken::new()
        )
        .await
        .err(),
        Some(TransferError::Integrity)
    );
}

#[tokio::test]
async fn cancelling_a_future_does_not_release_ongoing_filesystem_capacity() {
    let t = Transfer::new(1, Duration::from_secs(1)).unwrap();
    let (entered_tx, entered_rx) = tokio::sync::oneshot::channel();
    let (release_tx, release_rx) = std::sync::mpsc::channel();
    let transfer = t.clone();
    let first = tokio::spawn(async move {
        transfer
            .io_task(move || {
                let _ = entered_tx.send(());
                let _ = release_rx.recv();
                Ok(())
            })
            .await
    });
    entered_rx.await.unwrap();
    first.abort();
    assert!(first.await.unwrap_err().is_cancelled());
    assert_eq!(t.io_slots.available_permits(), 0);
    let idle = t.wait_io_idle();
    tokio::pin!(idle);
    assert!(tokio::time::timeout(Duration::from_millis(30), &mut idle)
        .await
        .is_err());
    let transfer = t.clone();
    let second = tokio::spawn(async move { transfer.io_task(|| Ok(())).await });
    release_tx.send(()).unwrap();
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(2), idle)
            .await
            .unwrap(),
        Ok(())
    );
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(2), second)
            .await
            .unwrap()
            .unwrap(),
        Ok(())
    );
    assert_eq!(t.io_slots.available_permits(), 1);
}

#[tokio::test]
async fn continuing_body_progress_extends_idle_time_and_preserves_signed_headers() {
    let data = vec![b'p'; 8192];
    let body = data.clone();
    let server = Server::new(move |request| {
        assert_eq!(
            request.headers.get("x-amz-test").map(String::as_str),
            Some("opaque-grant")
        );
        let mut reply = Reply::bytes(&body);
        reply.delay = Duration::from_millis(700);
        reply.body_pause = Some(Duration::from_millis(700));
        // Exercise a successful response without Content-Length as well.
        reply.length = None;
        reply
    })
    .await;
    let mut grant = server.grant("GET", data.len());
    let host = grant
        .url
        .split_once("://")
        .unwrap()
        .1
        .split('/')
        .next()
        .unwrap()
        .to_owned();
    grant.headers.insert("Host".into(), vec![host]);
    grant
        .headers
        .insert("x-amz-test".into(), vec!["opaque-grant".into()]);
    assert_eq!(
        transfer()
            .verify_remote(&grant, &archive(&data), CancellationToken::new())
            .await,
        Ok(())
    );
}
