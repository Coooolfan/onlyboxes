//! Ephemeral HTTP object-service fixture, not an S3 conformance implementation.
use chrono::Utc;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use worker_boxlite::persistence::{Archive, CompletedPart, TransferGrant};

pub struct Store {
    pub url: String,
    pub object: PathBuf,
    root: PathBuf,
    task: tokio::task::JoinHandle<()>,
}
impl Drop for Store {
    fn drop(&mut self) {
        self.task.abort();
    }
}
impl Store {
    pub async fn new(root: &Path) -> std::io::Result<Self> {
        use std::os::unix::fs::PermissionsExt;
        tokio::fs::create_dir(root).await?;
        std::fs::set_permissions(root, std::fs::Permissions::from_mode(0o700))?;
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let address = listener.local_addr()?;
        let state = Arc::new(root.to_owned());
        let captured = state.clone();
        let task = tokio::spawn(async move {
            loop {
                let Ok((socket, _)) = listener.accept().await else {
                    return;
                };
                let root = captured.clone();
                tokio::spawn(async move {
                    let _ = serve(socket, &root).await;
                });
            }
        });
        Ok(Self {
            url: format!("http://{address}"),
            object: root.join("object"),
            root: state.as_ref().clone(),
            task,
        })
    }
    /// The fixture coordinator completes multipart data after the upload worker
    /// exits. Workers receive only PUT-part and GET grants.
    pub fn complete(&self, parts: &[CompletedPart], archive: &Archive) -> std::io::Result<()> {
        use std::io::Write;
        use std::os::unix::fs::OpenOptionsExt;
        let mut sorted = parts.to_vec();
        sorted.sort_by_key(|p| p.part_number);
        let mut output = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&self.object)?;
        let mut total = 0;
        for part in sorted {
            let mut input =
                std::fs::File::open(self.root.join(format!("part-{}", part.part_number)))?;
            if input.metadata()?.len() != part.size_bytes as u64
                || part.etag != format!("\"part-{}\"", part.part_number)
            {
                return Err(std::io::Error::other("invalid multipart receipt"));
            }
            total += std::io::copy(&mut input, &mut output)?;
        }
        if total != archive.size_bytes as u64 {
            return Err(std::io::Error::other("multipart size mismatch"));
        }
        output.flush()?;
        output.sync_all()?;
        std::fs::File::open(&self.root)?.sync_all()
    }
}
pub fn grant(base: &str, method: &str, size: i64, offset: i64, part: Option<i32>) -> TransferGrant {
    let path = part
        .map(|n| format!("/part/{n}"))
        .unwrap_or_else(|| "/object".into());
    TransferGrant {
        url: format!("{base}{path}"),
        method: method.into(),
        headers: std::collections::BTreeMap::from([(
            "x-onlyboxes-fixture".into(),
            vec!["authorized".into()],
        )]),
        expires_at: Utc::now() + chrono::Duration::minutes(5),
        offset,
        size_bytes: size,
    }
}
async fn serve(mut socket: TcpStream, root: &Path) -> std::io::Result<()> {
    let mut bytes = Vec::new();
    let end = loop {
        let mut buf = [0; 8192];
        let n = socket.read(&mut buf).await?;
        if n == 0 {
            return Err(std::io::ErrorKind::UnexpectedEof.into());
        }
        bytes.extend(&buf[..n]);
        if let Some(i) = bytes.windows(4).position(|v| v == b"\r\n\r\n") {
            break i + 4;
        }
        if bytes.len() > 64 << 10 {
            return Err(std::io::Error::other("fixture headers too large"));
        }
    };
    let text = String::from_utf8_lossy(&bytes[..end]);
    let mut lines = text.split("\r\n");
    let mut first = lines
        .next()
        .ok_or_else(|| std::io::Error::other("request missing"))?
        .split_whitespace();
    let method = first.next().unwrap_or("");
    let path = first.next().unwrap_or("");
    let mut headers = std::collections::BTreeMap::new();
    for line in lines {
        if let Some((k, v)) = line.split_once(':') {
            headers.insert(k.to_ascii_lowercase(), v.trim());
        }
    }
    if headers.get("x-onlyboxes-fixture") != Some(&"authorized") {
        socket
            .write_all(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            .await?;
        return Ok(());
    }
    if method == "PUT" {
        let number = path
            .strip_prefix("/part/")
            .and_then(|s| s.parse::<i32>().ok())
            .filter(|n| (1..=10000).contains(n))
            .ok_or_else(|| std::io::Error::other("invalid part"))?;
        let length = headers
            .get("content-length")
            .and_then(|s| s.parse::<u64>().ok())
            .filter(|n| *n <= 64 << 20)
            .ok_or_else(|| std::io::Error::other("invalid part length"))?;
        let mut file = tokio::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(root.join(format!("part-{number}")))
            .await?;
        let body = &bytes[end..];
        if body.len() as u64 > length {
            return Err(std::io::Error::other("part overflow"));
        }
        file.write_all(body).await?;
        let mut remaining = length - body.len() as u64;
        while remaining > 0 {
            let mut buffer = vec![0; (remaining as usize).min(64 << 10)];
            let n = socket.read(&mut buffer).await?;
            if n == 0 {
                return Err(std::io::ErrorKind::UnexpectedEof.into());
            }
            file.write_all(&buffer[..n]).await?;
            remaining -= n as u64;
        }
        file.sync_all().await?;
        socket.write_all(format!("HTTP/1.1 200 OK\r\nETag: \"part-{number}\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").as_bytes()).await?;
    } else if method == "GET" && path == "/object" {
        use tokio::io::AsyncSeekExt;
        let mut file = tokio::fs::File::open(root.join("object")).await?;
        let length = file.metadata().await?.len();
        let offset = headers
            .get("range")
            .and_then(|s| s.strip_prefix("bytes="))
            .and_then(|s| s.strip_suffix('-'))
            .and_then(|s| s.parse::<u64>().ok())
            .unwrap_or(0);
        if offset >= length {
            return Err(std::io::Error::other("invalid range"));
        }
        let mut header = if offset == 0 {
            "HTTP/1.1 200 OK\r\n".to_owned()
        } else {
            format!(
                "HTTP/1.1 206 Partial Content\r\nContent-Range: bytes {}-{}/{}\r\n",
                offset,
                length - 1,
                length
            )
        };
        header.push_str(&format!(
            "Content-Length: {}\r\nConnection: close\r\n\r\n",
            length - offset
        ));
        socket.write_all(header.as_bytes()).await?;
        file.seek(std::io::SeekFrom::Start(offset)).await?;
        tokio::io::copy(&mut file, &mut socket).await?;
    } else {
        return Err(std::io::Error::other("unsupported fixture request"));
    }
    socket.shutdown().await
}
