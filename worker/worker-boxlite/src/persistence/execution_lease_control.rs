//! One bounded frame per private connection; native enforcement owns its own
//! thread and never depends on a client or an asynchronous control task.
use super::journal::{check_file, private_directory, private_options};
use super::{BootAuthorization, Error, ExecutionLease, ExecutionLeaseLedger, HostProcess};
use super::{LeaseRetirement, RetirementStatus};
use serde::{Deserialize, Serialize};
use std::fs::{self, File};
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{UnixListener, UnixStream};
use tokio::task::JoinSet;
use tokio_util::sync::CancellationToken;

type Result<T> = std::result::Result<T, Error>;
const VERSION: u32 = 1;
const MAX_FRAME: usize = 16 << 10;
const MAX_HANDLERS: usize = 8;
const IO_TIMEOUT: Duration = Duration::from_secs(1);

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LeaseGuardIdentity {
    pub worker_id: String,
    pub store_id: String,
    pub epoch_id: String,
    pub process: HostProcess,
}
impl LeaseGuardIdentity {
    pub(super) fn validate(&self) -> Result<()> {
        if self.worker_id.trim().is_empty()
            || self.worker_id.len() > 256
            || self.worker_id.trim() != self.worker_id
            || self.worker_id.chars().any(char::is_control)
        {
            return Err(Error::Unavailable);
        }
        for value in [&self.store_id, &self.epoch_id] {
            if !uuid::Uuid::parse_str(value)
                .is_ok_and(|id| !id.is_nil() && id.to_string() == *value)
            {
                return Err(Error::Unavailable);
            }
        }
        self.process.validate()
    }
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    version: u32,
    request_id: String,
    worker_id: String,
    store_id: Option<String>,
    expected: Option<LeaseGuardIdentity>,
    command: Command,
}
#[derive(Serialize, Deserialize)]
#[serde(tag = "action", rename_all = "snake_case", deny_unknown_fields)]
enum Command {
    PrepareRetirement {
        retirement: LeaseRetirement,
    },
    FinishRetirement {
        retirement: LeaseRetirement,
    },
    RetirementStatus {
        box_id: String,
    },
    CheckRunning {
        box_id: String,
    },
    PrepareBoot {
        authorization: BootAuthorization,
    },
    BindBoot {
        authorization: BootAuthorization,
        process: HostProcess,
    },
    BootStatus {
        box_id: String,
    },
    Ping,
    Status {
        box_id: String,
    },
    Adopt {
        lease: ExecutionLease,
    },
    Renew {
        expected_revision: u64,
        lease: ExecutionLease,
    },
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Response {
    version: u32,
    request_id: String,
    identity: LeaseGuardIdentity,
    reply: Reply,
}
#[derive(Serialize, Deserialize)]
#[serde(
    tag = "type",
    content = "value",
    rename_all = "snake_case",
    deny_unknown_fields
)]
enum Reply {
    Retired(LeaseRetirement),
    Released(LeaseRetirement),
    RetirementStatus(Option<RetirementStatus>),
    Running(ExecutionLease),
    BootPrepared(BootAuthorization),
    BootStatus(Option<BootAuthorization>),
    Ready,
    Status(Option<ExecutionLease>),
    Granted(ExecutionLease),
    Rejected(Failure),
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
enum Failure {
    InvalidRequest,
    Conflict,
    Unavailable,
}

fn private_parent(path: &Path) -> Result<PathBuf> {
    if !path.is_absolute() || path.file_name().is_none() {
        return Err(Error::Unavailable);
    }
    let parent = path.parent().ok_or(Error::Unavailable)?;
    let metadata = fs::symlink_metadata(parent).map_err(|_| Error::Unavailable)?;
    // SAFETY: geteuid only queries the current process identity.
    if !metadata.is_dir()
        || metadata.mode() & 0o077 != 0
        || metadata.uid() != unsafe { libc::geteuid() }
    {
        return Err(Error::Unavailable);
    }
    fs::canonicalize(parent).map_err(|_| Error::Unavailable)
}
fn private_socket(path: &Path) -> Result<fs::Metadata> {
    private_parent(path)?;
    let metadata = fs::symlink_metadata(path).map_err(|_| Error::Unavailable)?;
    // SAFETY: geteuid only queries the current process identity.
    if !metadata.file_type().is_socket()
        || metadata.mode() & 0o777 != 0o600
        || metadata.uid() != unsafe { libc::geteuid() }
        || metadata.nlink() != 1
    {
        return Err(Error::Unavailable);
    }
    Ok(metadata)
}
fn same_peer(stream: &UnixStream) -> Result<u32> {
    let credentials = stream.peer_cred().map_err(|_| Error::Unavailable)?;
    // SAFETY: geteuid only queries the current process identity.
    if credentials.uid() != unsafe { libc::geteuid() } {
        return Err(Error::Unavailable);
    }
    credentials
        .pid()
        .filter(|pid| *pid > 1)
        .map(|pid| pid as u32)
        .ok_or(Error::Unavailable)
}
async fn read_frame<T: for<'a> Deserialize<'a>>(stream: &mut UnixStream) -> Result<T> {
    tokio::time::timeout(IO_TIMEOUT, async {
        let length = stream.read_u32().await.map_err(|_| Error::Unavailable)? as usize;
        if length == 0 || length > MAX_FRAME {
            return Err(Error::Unavailable);
        }
        let mut bytes = vec![0; length];
        stream
            .read_exact(&mut bytes)
            .await
            .map_err(|_| Error::Unavailable)?;
        serde_json::from_slice(&bytes).map_err(|_| Error::Unavailable)
    })
    .await
    .map_err(|_| Error::Unavailable)?
}
async fn write_frame<T: Serialize>(stream: &mut UnixStream, value: &T) -> Result<()> {
    let bytes = serde_json::to_vec(value).map_err(|_| Error::Unavailable)?;
    if bytes.is_empty() || bytes.len() > MAX_FRAME {
        return Err(Error::Unavailable);
    }
    tokio::time::timeout(IO_TIMEOUT, async {
        stream
            .write_u32(bytes.len() as u32)
            .await
            .map_err(|_| Error::Unavailable)?;
        stream
            .write_all(&bytes)
            .await
            .map_err(|_| Error::Unavailable)
    })
    .await
    .map_err(|_| Error::Unavailable)?
}

struct SocketOwner {
    path: PathBuf,
    device: u64,
    inode: u64,
    lock: File,
}
impl Drop for SocketOwner {
    fn drop(&mut self) {
        if fs::symlink_metadata(&self.path).is_ok_and(|meta| {
            meta.file_type().is_socket() && meta.dev() == self.device && meta.ino() == self.inode
        }) {
            let _ = fs::remove_file(&self.path);
        }
        let _ = self.lock.unlock();
    }
}

pub struct LeaseGuardServer {
    listener: UnixListener,
    socket: SocketOwner,
    ledger: Arc<ExecutionLeaseLedger>,
    identity: LeaseGuardIdentity,
}
impl LeaseGuardServer {
    /// The control directory is separate from the ledger and must be private.
    /// A stale endpoint is removed only under its exclusive control lock.
    pub async fn bind(
        ledger: Arc<ExecutionLeaseLedger>,
        socket_dir: impl AsRef<Path>,
    ) -> Result<Self> {
        ledger.control_available()?;
        ledger.claim_control()?;
        if !socket_dir.as_ref().is_absolute() {
            return Err(Error::Unavailable);
        }
        private_directory(socket_dir.as_ref())?;
        let directory = fs::canonicalize(socket_dir).map_err(|_| Error::Unavailable)?;
        if !ledger.separate_control_dir(&directory) {
            return Err(Error::Unavailable);
        }
        let path = directory.join("control.sock");
        private_parent(&path)?;
        let lock = private_options()
            .read(true)
            .write(true)
            .create(true)
            .open(directory.join("control.lock"))
            .map_err(|_| Error::Unavailable)?;
        check_file(&lock, 0)?;
        lock.try_lock().map_err(|_| Error::Unavailable)?;
        match fs::symlink_metadata(&path) {
            Ok(_) => {
                let before = private_socket(&path)?;
                match tokio::time::timeout(IO_TIMEOUT, UnixStream::connect(&path)).await {
                    Ok(Err(error)) if error.kind() == std::io::ErrorKind::ConnectionRefused => {}
                    _ => return Err(Error::Unavailable),
                }
                let after = private_socket(&path)?;
                if before.dev() != after.dev() || before.ino() != after.ino() {
                    return Err(Error::Unavailable);
                }
                fs::remove_file(&path).map_err(|_| Error::Unavailable)?;
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => return Err(Error::Unavailable),
        }
        let listener = UnixListener::bind(&path).map_err(|_| Error::Unavailable)?;
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600))
            .map_err(|_| Error::Unavailable)?;
        let metadata = private_socket(&path)?;
        let identity = LeaseGuardIdentity {
            worker_id: ledger.worker_id().into(),
            store_id: ledger.store_id().into(),
            epoch_id: uuid::Uuid::new_v4().to_string(),
            process: HostProcess::capture(std::process::id())?,
        };
        Ok(Self {
            listener,
            socket: SocketOwner {
                path,
                device: metadata.dev(),
                inode: metadata.ino(),
                lock,
            },
            ledger,
            identity,
        })
    }

    pub fn identity(&self) -> &LeaseGuardIdentity {
        &self.identity
    }

    pub async fn run(self, shutdown: CancellationToken) -> Result<()> {
        let active = Arc::new(AtomicUsize::new(0));
        let enforcement = Enforcer::start(self.ledger.clone(), active.clone(), shutdown.clone());
        self.run_owned(shutdown, active, enforcement).await
    }

    /// Production service entry: begin enforcement before endpoint setup, so
    /// a socket/configuration failure cannot leave loaded writers unhandled.
    pub async fn serve(
        ledger: Arc<ExecutionLeaseLedger>,
        socket_dir: impl AsRef<Path>,
        shutdown: CancellationToken,
    ) -> Result<()> {
        let active = Arc::new(AtomicUsize::new(0));
        let mut enforcement = Enforcer::start(ledger.clone(), active.clone(), shutdown.clone());
        match Self::bind(ledger.clone(), socket_dir).await {
            Ok(server) => server.run_owned(shutdown, active, enforcement).await,
            Err(error) => {
                ledger.close_admission();
                enforcement.closing.store(true, Ordering::SeqCst);
                enforcement.join().await?;
                Err(error)
            }
        }
    }

    async fn run_owned(
        self,
        shutdown: CancellationToken,
        active: Arc<AtomicUsize>,
        mut enforcement: Enforcer,
    ) -> Result<()> {
        let Self {
            listener,
            socket,
            ledger,
            identity,
        } = self;
        let mut handlers = JoinSet::new();
        let mut result = Ok(());
        // Readiness requires a completed enforcement pass, not only a bound
        // listener or a successfully allocated thread handle.
        while !enforcement.started.load(Ordering::SeqCst) && !shutdown.is_cancelled() {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        loop {
            if shutdown.is_cancelled() {
                break;
            }
            tokio::select! {
                _=shutdown.cancelled()=>break,
                completed=handlers.join_next(),if !handlers.is_empty()=>{
                    if completed.is_some_and(|result|result.is_err()) {result=Err(Error::Unavailable);break;}
                },
                accepted=listener.accept()=>{
                    let (stream,_)=match accepted {Ok(value)=>value,Err(_)=>{result=Err(Error::Unavailable);break;}};
                    if handlers.len()>=MAX_HANDLERS || same_peer(&stream).is_err() {continue;}
                    let ledger=ledger.clone(); let identity=identity.clone(); let active=active.clone();
                    handlers.spawn(async move {let _=handle(stream,ledger,identity,active).await;});
                }
            }
        }
        ledger.close_admission();
        enforcement.closing.store(true, Ordering::SeqCst);
        drop(listener);
        while let Some(completed) = handlers.join_next().await {
            if completed.is_err() {
                result = Err(Error::Unavailable);
            }
        }
        enforcement.join().await?;
        drop(socket);
        result
    }
}

struct ActiveJob(Arc<AtomicUsize>);
impl ActiveJob {
    fn new(active: Arc<AtomicUsize>) -> Self {
        active.fetch_add(1, Ordering::SeqCst);
        Self(active)
    }
}
impl Drop for ActiveJob {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::SeqCst);
    }
}
struct Enforcer {
    ledger: Arc<ExecutionLeaseLedger>,
    closing: Arc<AtomicBool>,
    started: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
    discovery: Option<std::thread::JoinHandle<()>>,
}
struct EnforcementExit {
    ledger: Arc<ExecutionLeaseLedger>,
    shutdown: CancellationToken,
}
impl Drop for EnforcementExit {
    fn drop(&mut self) {
        self.ledger.close_admission();
        self.shutdown.cancel();
    }
}
impl Enforcer {
    fn start(
        ledger: Arc<ExecutionLeaseLedger>,
        active: Arc<AtomicUsize>,
        shutdown: CancellationToken,
    ) -> Self {
        let closing = Arc::new(AtomicBool::new(false));
        let end = closing.clone();
        let engine = ledger.clone();
        let started = Arc::new(AtomicBool::new(false));
        let ready = started.clone();
        let boot_ready = Arc::new(AtomicBool::new(false));
        let boot_ready_writer = boot_ready.clone();
        let boot_engine = ledger.clone();
        let boot_shutdown = shutdown.clone();
        let boot_end = closing.clone();
        let boot_active = active.clone();
        let discovery = std::thread::spawn(move || {
            let _exit = EnforcementExit {
                ledger: boot_engine.clone(),
                shutdown: boot_shutdown.clone(),
            };
            loop {
                if boot_engine.enforce_boots().is_err() {
                    boot_engine.close_admission();
                    boot_shutdown.cancel();
                }
                boot_ready_writer.store(true, Ordering::SeqCst);
                if boot_end.load(Ordering::SeqCst)
                    && boot_active.load(Ordering::SeqCst) == 0
                    && boot_engine.boots_settled() == Ok(true)
                {
                    break;
                }
                std::thread::sleep(Duration::from_millis(25));
            }
        });
        let thread = std::thread::spawn(move || {
            let _exit = EnforcementExit {
                ledger: engine.clone(),
                shutdown: shutdown.clone(),
            };
            loop {
                if end.load(Ordering::SeqCst) {
                    engine.close_admission();
                }
                if engine.enforce().is_err() {
                    engine.close_admission();
                    shutdown.cancel();
                } else if boot_ready.load(Ordering::SeqCst) {
                    ready.store(true, Ordering::SeqCst);
                }
                if end.load(Ordering::SeqCst)
                    && active.load(Ordering::SeqCst) == 0
                    && engine.writers_stopped() == Ok(true)
                {
                    break;
                }
                std::thread::sleep(Duration::from_millis(25));
            }
        });
        Self {
            ledger,
            closing,
            started,
            thread: Some(thread),
            discovery: Some(discovery),
        }
    }
    async fn join(&mut self) -> Result<()> {
        let thread = self.thread.take().ok_or(Error::Unavailable)?;
        let discovery = self.discovery.take().ok_or(Error::Unavailable)?;
        tokio::task::spawn_blocking(move || {
            let a = thread.join();
            let b = discovery.join();
            a.and(b)
        })
        .await
        .map_err(|_| Error::Unavailable)?
        .map_err(|_| Error::Unavailable)
    }
}
impl Drop for Enforcer {
    fn drop(&mut self) {
        self.ledger.close_admission();
        self.closing.store(true, Ordering::SeqCst);
    }
}

async fn handle(
    mut stream: UnixStream,
    ledger: Arc<ExecutionLeaseLedger>,
    identity: LeaseGuardIdentity,
    active: Arc<AtomicUsize>,
) -> Result<()> {
    let request: Request = read_frame(&mut stream).await?;
    let request_id = request.request_id.clone();
    let valid_nonce = uuid::Uuid::parse_str(&request_id)
        .is_ok_and(|id| !id.is_nil() && id.to_string() == request_id);
    let bound = request.expected.as_ref() == Some(&identity)
        && request.store_id.as_deref() == Some(&identity.store_id);
    let discovery = matches!(request.command, Command::Ping) && request.expected.is_none();
    let reply = if request.version != VERSION || !valid_nonce {
        Reply::Rejected(Failure::InvalidRequest)
    } else if request.worker_id != identity.worker_id
        || request
            .store_id
            .as_ref()
            .is_some_and(|store| store != &identity.store_id)
        || !(bound || discovery)
    {
        Reply::Rejected(Failure::Unavailable)
    } else {
        let job = ActiveJob::new(active);
        tokio::task::spawn_blocking(move || {
            let _job = job;
            let result = match request.command {
                Command::PrepareRetirement { retirement } => {
                    ledger.prepare_retirement(retirement).map(Reply::Retired)
                }
                Command::FinishRetirement { retirement } => {
                    ledger.finish_retirement(retirement).map(Reply::Released)
                }
                Command::RetirementStatus { box_id } => ledger
                    .retirement_status(&box_id)
                    .map(Reply::RetirementStatus),
                Command::CheckRunning { box_id } => {
                    ledger.check_running(&box_id).map(Reply::Running)
                }
                Command::PrepareBoot { authorization } => {
                    ledger.prepare_boot(authorization).map(Reply::BootPrepared)
                }
                Command::BindBoot {
                    authorization,
                    process,
                } => ledger.bind_boot(authorization, process).map(Reply::Granted),
                Command::BootStatus { box_id } => {
                    ledger.boot_status(&box_id).map(Reply::BootStatus)
                }
                Command::Ping => ledger.control_available().map(|_| Reply::Ready),
                Command::Status { box_id } => ledger.status(&box_id).map(Reply::Status),
                Command::Adopt { lease } => ledger.adopt(lease).map(Reply::Granted),
                Command::Renew {
                    expected_revision,
                    lease,
                } => ledger.renew(expected_revision, lease).map(Reply::Granted),
            };
            result.unwrap_or_else(|error| {
                Reply::Rejected(match error {
                    Error::Conflict => Failure::Conflict,
                    Error::Unavailable => Failure::Unavailable,
                })
            })
        })
        .await
        .map_err(|_| Error::Unavailable)?
    };
    write_frame(
        &mut stream,
        &Response {
            version: VERSION,
            request_id,
            identity,
            reply,
        },
    )
    .await
}

#[derive(Clone)]
pub struct LeaseGuardClient {
    socket: PathBuf,
    identity: LeaseGuardIdentity,
}
impl LeaseGuardClient {
    /// Discovery is read-only. Existing Worker bindings must pass their pinned
    /// store ID, and retain the returned epoch for every subsequent mutation.
    pub async fn discover(
        socket: impl AsRef<Path>,
        worker: &str,
        store_id: Option<&str>,
    ) -> Result<LeaseGuardIdentity> {
        let request = Request {
            version: VERSION,
            request_id: uuid::Uuid::new_v4().to_string(),
            worker_id: worker.into(),
            store_id: store_id.map(str::to_owned),
            expected: None,
            command: Command::Ping,
        };
        let response = exchange(socket.as_ref(), &request).await?;
        if !matches!(response.reply, Reply::Ready) {
            return Err(Error::Unavailable);
        }
        Ok(response.identity)
    }
    pub fn bind(socket: impl AsRef<Path>, identity: LeaseGuardIdentity) -> Self {
        Self {
            socket: socket.as_ref().to_owned(),
            identity,
        }
    }
    pub fn identity(&self) -> &LeaseGuardIdentity {
        &self.identity
    }
    async fn call(&self, command: Command) -> Result<Reply> {
        let response = exchange(
            &self.socket,
            &Request {
                version: VERSION,
                request_id: uuid::Uuid::new_v4().to_string(),
                worker_id: self.identity.worker_id.clone(),
                store_id: Some(self.identity.store_id.clone()),
                expected: Some(self.identity.clone()),
                command,
            },
        )
        .await?;
        match response.reply {
            Reply::Rejected(Failure::Conflict) => Err(Error::Conflict),
            Reply::Rejected(_) => Err(Error::Unavailable),
            reply => Ok(reply),
        }
    }
    pub async fn prepare_boot(
        &self,
        authorization: BootAuthorization,
    ) -> Result<BootAuthorization> {
        authorization.validate()?;
        match self
            .call(Command::PrepareBoot {
                authorization: authorization.clone(),
            })
            .await?
        {
            Reply::BootPrepared(grant) if grant == authorization => {
                let now = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map_err(|_| Error::Unavailable)?
                    .as_millis();
                if grant.expires_unix_ms as u128 <= now {
                    return Err(Error::Unavailable);
                }
                Ok(grant)
            }
            _ => Err(Error::Unavailable),
        }
    }
    pub async fn boot_status(&self, id: &str) -> Result<Option<BootAuthorization>> {
        match self.call(Command::BootStatus { box_id: id.into() }).await? {
            Reply::BootStatus(grant) => {
                if let Some(grant) = &grant {
                    grant.validate()?;
                    if grant.box_id != id {
                        return Err(Error::Unavailable);
                    }
                }
                Ok(grant)
            }
            _ => Err(Error::Unavailable),
        }
    }
    pub async fn bind_boot(
        &self,
        authorization: BootAuthorization,
        process: HostProcess,
    ) -> Result<ExecutionLease> {
        let wanted = ExecutionLease {
            box_id: authorization.box_id.clone(),
            scoped_session_id: authorization.scoped_session_id.clone(),
            generation: authorization.generation,
            revision: authorization
                .prior_revision
                .unwrap_or(0)
                .checked_add(1)
                .ok_or(Error::Conflict)?,
            expires_unix_ms: authorization.expires_unix_ms,
            process: process.clone(),
            revoked: false,
        };
        self.grant(
            Command::BindBoot {
                authorization,
                process,
            },
            wanted,
        )
        .await
    }
    pub async fn check_running(&self, id: &str) -> Result<ExecutionLease> {
        match self
            .call(Command::CheckRunning { box_id: id.into() })
            .await?
        {
            Reply::Running(lease) => {
                lease.validate()?;
                let now = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map_err(|_| Error::Unavailable)?
                    .as_millis();
                if lease.box_id != id || lease.revoked || lease.expires_unix_ms as u128 <= now {
                    return Err(Error::Unavailable);
                }
                Ok(lease)
            }
            _ => Err(Error::Unavailable),
        }
    }
    pub async fn retirement_status(&self, id: &str) -> Result<Option<RetirementStatus>> {
        match self
            .call(Command::RetirementStatus { box_id: id.into() })
            .await?
        {
            Reply::RetirementStatus(status) => {
                if let Some(status) = &status {
                    status.retirement.validate()?;
                    if status.retirement.box_id != id {
                        return Err(Error::Unavailable);
                    }
                }
                Ok(status)
            }
            _ => Err(Error::Unavailable),
        }
    }
    pub async fn prepare_retirement(&self, retirement: LeaseRetirement) -> Result<LeaseRetirement> {
        retirement.validate()?;
        match self
            .call(Command::PrepareRetirement {
                retirement: retirement.clone(),
            })
            .await?
        {
            Reply::Retired(receipt) if receipt == retirement => Ok(receipt),
            _ => Err(Error::Unavailable),
        }
    }
    pub async fn finish_retirement(&self, retirement: LeaseRetirement) -> Result<LeaseRetirement> {
        retirement.validate()?;
        match self
            .call(Command::FinishRetirement {
                retirement: retirement.clone(),
            })
            .await?
        {
            Reply::Released(receipt) if receipt == retirement => Ok(receipt),
            _ => Err(Error::Unavailable),
        }
    }
    pub async fn status(&self, id: &str) -> Result<Option<ExecutionLease>> {
        match self.call(Command::Status { box_id: id.into() }).await? {
            Reply::Status(lease) => {
                if let Some(lease) = &lease {
                    lease.validate()?;
                    if lease.box_id != id {
                        return Err(Error::Unavailable);
                    }
                }
                Ok(lease)
            }
            _ => Err(Error::Unavailable),
        }
    }
    pub async fn adopt(&self, lease: ExecutionLease) -> Result<ExecutionLease> {
        self.grant(
            Command::Adopt {
                lease: lease.clone(),
            },
            lease,
        )
        .await
    }
    pub async fn renew(
        &self,
        expected_revision: u64,
        lease: ExecutionLease,
    ) -> Result<ExecutionLease> {
        self.grant(
            Command::Renew {
                expected_revision,
                lease: lease.clone(),
            },
            lease,
        )
        .await
    }
    async fn grant(&self, command: Command, wanted: ExecutionLease) -> Result<ExecutionLease> {
        wanted.validate()?;
        match self.call(command).await? {
            Reply::Granted(lease) if lease == wanted && !lease.revoked => {
                let now = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map_err(|_| Error::Unavailable)?
                    .as_millis();
                if lease.expires_unix_ms as u128 <= now {
                    return Err(Error::Unavailable);
                }
                Ok(lease)
            }
            _ => Err(Error::Unavailable),
        }
    }
}
async fn exchange(path: &Path, request: &Request) -> Result<Response> {
    tokio::time::timeout(Duration::from_secs(3), async {
        let owned = path.to_owned();
        tokio::task::spawn_blocking(move || private_socket(&owned))
            .await
            .map_err(|_| Error::Unavailable)??;
        let mut stream = UnixStream::connect(path)
            .await
            .map_err(|_| Error::Unavailable)?;
        let peer = same_peer(&stream)?;
        let process = tokio::task::spawn_blocking(move || HostProcess::capture(peer))
            .await
            .map_err(|_| Error::Unavailable)??;
        if request
            .expected
            .as_ref()
            .is_some_and(|expected| expected.process != process)
        {
            return Err(Error::Unavailable);
        }
        write_frame(&mut stream, request).await?;
        let response: Response = read_frame(&mut stream).await?;
        response.identity.validate()?;
        if response.version != VERSION
            || response.request_id != request.request_id
            || response.identity.process != process
            || response.identity.worker_id != request.worker_id
            || request
                .store_id
                .as_ref()
                .is_some_and(|store| store != &response.identity.store_id)
            || request
                .expected
                .as_ref()
                .is_some_and(|expected| expected != &response.identity)
        {
            return Err(Error::Unavailable);
        }
        Ok(response)
    })
    .await
    .map_err(|_| Error::Unavailable)?
}

#[cfg(test)]
#[path = "execution_lease_control_tests.rs"]
mod tests;
