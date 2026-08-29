use std::io;
use std::net::{IpAddr, SocketAddr};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use base64::Engine;
use hmac::{Hmac, Mac};
use serde::Deserialize;
use sha2::Sha256;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio_util::sync::CancellationToken;

use crate::config::Config;
use crate::{boxlite_runtime, boxlite_runtime::BoxliteCommandError};

use super::terminal_session_manager::{ProxyTargetError, TerminalSessionManager};
use super::RunnerError;

const TOKEN_PREFIX: &str = "obx_route_v1.";
const TOKEN_HEADER: &str = "x-onlyboxes-route-token";
const KEY_DERIVATION_LABEL: &[u8] = b"onlyboxes/proxy-route/v1";
const MAX_HEADER_BYTES: usize = 1 << 20;
const HEADER_TIMEOUT: Duration = Duration::from_secs(5);

type HmacSha256 = Hmac<Sha256>;

#[derive(Deserialize)]
struct RouteClaims {
    worker_id: String,
    session_id: String,
    port: u16,
    exp: i64,
}

pub(crate) fn validate_config(cfg: &Config) -> Result<(), RunnerError> {
    if !cfg.proxy_enabled {
        return Ok(());
    }
    let listen: SocketAddr = cfg.proxy_listen_addr.trim().parse().map_err(|_| {
        RunnerError::Message("WORKER_PROXY_LISTEN_ADDR must be an IP address with port".to_owned())
    })?;
    let advertise: SocketAddr = cfg.proxy_advertise_addr.trim().parse().map_err(|_| {
        RunnerError::Message(
            "WORKER_PROXY_ADVERTISE_ADDR must be an IP address with port".to_owned(),
        )
    })?;
    if listen.port() != advertise.port() {
        return Err(RunnerError::Message(
            "proxy listen and advertise ports must match".to_owned(),
        ));
    }
    if !is_unicast(advertise.ip()) {
        return Err(RunnerError::Message(
            "WORKER_PROXY_ADVERTISE_ADDR must use a unicast IP".to_owned(),
        ));
    }
    if cfg.proxy_sandbox_ports.is_empty() {
        return Err(RunnerError::Message(
            "WORKER_PROXY_SANDBOX_PORTS must contain at least one port".to_owned(),
        ));
    }
    Ok(())
}

fn is_unicast(ip: IpAddr) -> bool {
    if ip.is_unspecified() || ip.is_loopback() || ip.is_multicast() {
        return false;
    }
    match ip {
        IpAddr::V4(address) => !address.is_link_local() && !address.is_broadcast(),
        IpAddr::V6(address) => !address.is_unicast_link_local(),
    }
}

pub(crate) async fn run(
    shutdown: CancellationToken,
    cfg: Config,
    manager: Arc<TerminalSessionManager>,
) -> Result<(), RunnerError> {
    let listener = TcpListener::bind(cfg.proxy_listen_addr.trim())
        .await
        .map_err(|err| RunnerError::Message(format!("bind sandbox proxy: {err}")))?;
    let key = derive_key(cfg.worker_secret.trim())?;
    let cfg = Arc::new(cfg);
    tracing::info!(listen_addr = %cfg.proxy_listen_addr, advertise_addr = %cfg.proxy_advertise_addr, "sandbox proxy listening");
    loop {
        tokio::select! {
            _ = shutdown.cancelled() => return Err(RunnerError::Cancelled),
            accepted = listener.accept() => {
                let (stream, _) = accepted.map_err(|err| RunnerError::Message(format!("accept sandbox proxy connection: {err}")))?;
                let manager = manager.clone();
                let worker_id = cfg.worker_id.clone();
                let cfg = cfg.clone();
                let key = key.clone();
                let shutdown = shutdown.clone();
                tokio::spawn(async move {
                    if let Err(err) = handle_connection(stream, worker_id, key, cfg, manager, shutdown).await {
                        tracing::debug!(error = %err, "sandbox proxy connection closed");
                    }
                });
            }
        }
    }
}

fn derive_key(secret: &str) -> Result<Vec<u8>, RunnerError> {
    if secret.is_empty() {
        return Err(RunnerError::Message("WORKER_SECRET is required".to_owned()));
    }
    let mut mac = HmacSha256::new_from_slice(secret.as_bytes())
        .map_err(|_| RunnerError::Message("invalid worker secret".to_owned()))?;
    mac.update(KEY_DERIVATION_LABEL);
    Ok(mac.finalize().into_bytes().to_vec())
}

async fn handle_connection(
    mut client: TcpStream,
    worker_id: String,
    key: Vec<u8>,
    cfg: Arc<Config>,
    manager: Arc<TerminalSessionManager>,
    shutdown: CancellationToken,
) -> io::Result<()> {
    let header = match tokio::time::timeout(HEADER_TIMEOUT, read_header(&mut client)).await {
        Ok(Ok(header)) => header,
        Ok(Err(err)) => return Err(err),
        Err(_) => return write_error(&mut client, 408, "request timeout").await,
    };
    let (cleaned, token) = match sanitize_header(&header) {
        Ok(value) => value,
        Err(status) => return write_error(&mut client, status, "invalid route token").await,
    };
    let claims = match verify_token(&key, &token) {
        Ok(claims) if claims.worker_id == worker_id => claims,
        _ => return write_error(&mut client, 401, "invalid route token").await,
    };
    let box_id = match manager
        .resolve_proxy_target(&claims.session_id, claims.port, SystemTime::now())
        .await
    {
        Ok(target) => target,
        Err(err @ ProxyTargetError::SessionNotFound) => {
            return write_error(&mut client, 404, &err.to_string()).await
        }
        Err(err @ ProxyTargetError::PortNotEnabled { .. }) => {
            return write_error(&mut client, 403, &err.to_string()).await
        }
    };
    let mut upstream =
        match boxlite_runtime::open_terminal_proxy_connection(&cfg, &box_id, claims.port).await {
            Ok(connection) => connection,
            Err(BoxliteCommandError::MissingBox) => {
                return write_error(&mut client, 404, "sandbox session not found").await
            }
            Err(_) => return write_error(&mut client, 502, "sandbox upstream unavailable").await,
        };
    upstream.write_all(&cleaned).await?;

    let session_id = claims.session_id;
    tokio::select! {
        result = tokio::io::copy_bidirectional(&mut client, &mut upstream) => result.map(|_| ()),
        _ = shutdown.cancelled() => Ok(()),
        _ = wait_session_end(manager, session_id) => Ok(()),
    }
}

async fn wait_session_end(manager: Arc<TerminalSessionManager>, session_id: String) {
    loop {
        tokio::time::sleep(Duration::from_secs(1)).await;
        if !manager
            .proxy_session_active(&session_id, SystemTime::now())
            .await
        {
            return;
        }
    }
}

async fn read_header(stream: &mut TcpStream) -> io::Result<Vec<u8>> {
    let mut data = Vec::with_capacity(4096);
    let mut chunk = [0_u8; 4096];
    loop {
        let read = stream.read(&mut chunk).await?;
        if read == 0 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "request header ended early",
            ));
        }
        data.extend_from_slice(&chunk[..read]);
        if data.windows(4).any(|window| window == b"\r\n\r\n") {
            return Ok(data);
        }
        if data.len() > MAX_HEADER_BYTES {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "request header too large",
            ));
        }
    }
}

fn sanitize_header(data: &[u8]) -> Result<(Vec<u8>, String), u16> {
    let boundary = data
        .windows(4)
        .position(|window| window == b"\r\n\r\n")
        .ok_or(400_u16)?
        + 4;
    if boundary > MAX_HEADER_BYTES {
        return Err(400);
    }
    let head = std::str::from_utf8(&data[..boundary]).map_err(|_| 400_u16)?;
    let mut lines = head[..head.len() - 4].split("\r\n");
    let request_line = lines.next().ok_or(400_u16)?;
    if request_line
        .split_whitespace()
        .next()
        .is_some_and(|method| method.eq_ignore_ascii_case("CONNECT"))
    {
        return Err(405);
    }
    let mut token = None;
    let mut clean = String::with_capacity(head.len());
    clean.push_str(request_line);
    clean.push_str("\r\n");
    for line in lines {
        let (name, value) = line.split_once(':').ok_or(400_u16)?;
        let normalized = name.trim().to_ascii_lowercase();
        if normalized == TOKEN_HEADER {
            if token.replace(value.trim().to_owned()).is_some() {
                return Err(401);
            }
            continue;
        }
        if matches!(
            normalized.as_str(),
            "x-onlyboxes-internal-token"
                | "x-onlyboxes-upstream"
                | "x-onlyboxes-upstream-host"
                | "x-onlyboxes-upstream-traffic-token"
                | "x-original-host"
        ) {
            continue;
        }
        clean.push_str(line);
        clean.push_str("\r\n");
    }
    clean.push_str("\r\n");
    let mut output = clean.into_bytes();
    output.extend_from_slice(&data[boundary..]);
    Ok((
        output,
        token.filter(|value| !value.is_empty()).ok_or(401_u16)?,
    ))
}

fn verify_token(key: &[u8], token: &str) -> Result<RouteClaims, ()> {
    if token.len() > 4096 {
        return Err(());
    }
    let value = token.strip_prefix(TOKEN_PREFIX).ok_or(())?;
    let (payload, signature) = value.split_once('.').ok_or(())?;
    if signature.contains('.') || payload.is_empty() || signature.is_empty() {
        return Err(());
    }
    let signature = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(signature)
        .map_err(|_| ())?;
    let mut mac = HmacSha256::new_from_slice(key).map_err(|_| ())?;
    mac.update(format!("{TOKEN_PREFIX}{payload}").as_bytes());
    mac.verify_slice(&signature).map_err(|_| ())?;
    let payload = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(payload)
        .map_err(|_| ())?;
    let claims: RouteClaims = serde_json::from_slice(&payload).map_err(|_| ())?;
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| ())?
        .as_millis() as i64;
    if claims.worker_id.trim().is_empty()
        || claims.session_id.trim().is_empty()
        || claims.port == 0
        || claims.exp <= now
    {
        return Err(());
    }
    Ok(claims)
}

async fn write_error(stream: &mut TcpStream, status: u16, message: &str) -> io::Result<()> {
    let reason = match status {
        400 => "Bad Request",
        401 => "Unauthorized",
        403 => "Forbidden",
        404 => "Not Found",
        405 => "Method Not Allowed",
        408 => "Request Timeout",
        502 => "Bad Gateway",
        _ => "Error",
    };
    let body = format!("{message}\n");
    let response = format!("HTTP/1.1 {status} {reason}\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
    stream.write_all(response.as_bytes()).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::engine::general_purpose::URL_SAFE_NO_PAD;
    use serde::Serialize;

    #[derive(Serialize)]
    struct TestClaims<'a> {
        worker_id: &'a str,
        session_id: &'a str,
        port: u16,
        exp: i64,
    }

    fn signed_token(key: &[u8], expires_at: i64) -> String {
        let payload = URL_SAFE_NO_PAD.encode(
            serde_json::to_vec(&TestClaims {
                worker_id: "worker-a",
                session_id: "obx:owner:session",
                port: 8080,
                exp: expires_at,
            })
            .unwrap(),
        );
        let signed = format!("{TOKEN_PREFIX}{payload}");
        let mut mac = HmacSha256::new_from_slice(key).unwrap();
        mac.update(signed.as_bytes());
        format!(
            "{signed}.{}",
            URL_SAFE_NO_PAD.encode(mac.finalize().into_bytes())
        )
    }

    #[test]
    fn route_token_matches_go_contract() {
        let key = derive_key("worker-secret").unwrap();
        let expires_at = 4_102_444_800_000;
        let token = signed_token(&key, expires_at);
        assert_eq!(
            token,
            "obx_route_v1.eyJ3b3JrZXJfaWQiOiJ3b3JrZXItYSIsInNlc3Npb25faWQiOiJvYng6b3duZXI6c2Vzc2lvbiIsInBvcnQiOjgwODAsImV4cCI6NDEwMjQ0NDgwMDAwMH0.Ed5xO4kv0pF1fEyvuqn33UVFDJbZF_z_8tpX7ChTvlk"
        );
        let claims = verify_token(&key, &token).unwrap();
        assert_eq!(claims.worker_id, "worker-a");
        assert_eq!(claims.session_id, "obx:owner:session");
        assert_eq!(claims.port, 8080);
    }

    #[test]
    fn sanitize_header_removes_internal_headers_and_preserves_body() {
        let request = b"POST /api HTTP/1.1\r\nHost: preview.test\r\nX-Onlyboxes-Route-Token: signed\r\nX-Onlyboxes-Upstream: forged\r\nContent-Length: 4\r\n\r\nbody";
        let (cleaned, token) = sanitize_header(request).unwrap();
        let cleaned = String::from_utf8(cleaned).unwrap();
        assert_eq!(token, "signed");
        assert!(!cleaned.to_ascii_lowercase().contains("x-onlyboxes-"));
        assert!(cleaned.ends_with("\r\n\r\nbody"));
        assert!(cleaned.contains("Host: preview.test"));
    }

    #[test]
    fn sanitize_header_rejects_duplicate_token_and_connect() {
        assert_eq!(sanitize_header(b"GET / HTTP/1.1\r\nX-Onlyboxes-Route-Token: a\r\nX-Onlyboxes-Route-Token: b\r\n\r\n").unwrap_err(), 401);
        assert_eq!(
            sanitize_header(b"CONNECT target HTTP/1.1\r\nX-Onlyboxes-Route-Token: a\r\n\r\n")
                .unwrap_err(),
            405
        );
    }

    #[test]
    fn advertise_address_accepts_private_unicast_only() {
        assert!(is_unicast("10.20.1.16".parse().unwrap()));
        for address in [
            "0.0.0.0",
            "127.0.0.1",
            "169.254.1.1",
            "224.0.0.1",
            "::1",
            "fe80::1",
        ] {
            assert!(!is_unicast(address.parse().unwrap()), "accepted {address}");
        }
    }
}
