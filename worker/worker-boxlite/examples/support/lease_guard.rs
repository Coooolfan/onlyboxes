//! An owned companion for native acceptance fixtures. Production supervision
//! is configured separately from these test-only child process helpers.
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Child, Command},
    time::{Duration, Instant},
};
use worker_boxlite::persistence::LeaseGuardClient;
type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
pub struct Guard {
    pub client: LeaseGuardClient,
    process: Child,
    control: PathBuf,
}
impl Guard {
    pub async fn start(root: &Path, worker: &str) -> Result<Self> {
        let helper = std::env::var_os("BOXLITE_LEASE_GUARD_BINARY")
            .map(PathBuf::from)
            .unwrap_or_else(|| {
                std::env::current_exe()
                    .unwrap()
                    .parent()
                    .unwrap()
                    .parent()
                    .unwrap()
                    .join("boxlite-lease-guard")
            });
        if !helper.is_file() {
            return Err("build --bin boxlite-lease-guard before native control acceptance".into());
        }
        let control = PathBuf::from("/tmp")
            .join(format!("obx-lc-{}", &uuid::Uuid::new_v4().to_string()[..8]));
        fs::create_dir(&control)?;
        fs::set_permissions(&control, fs::Permissions::from_mode(0o700))?;
        let mut process = Command::new(helper)
            .arg("--state-dir")
            .arg(root.join("execution-leases"))
            .arg("--socket-dir")
            .arg(&control)
            .arg("--worker-id")
            .arg(worker)
            .spawn()?;
        let deadline = Instant::now() + Duration::from_secs(10);
        let identity = loop {
            if process.try_wait()?.is_some() || Instant::now() > deadline {
                let _ = process.kill();
                let _ = process.wait();
                let _ = fs::remove_dir_all(&control);
                return Err("owned guardian did not become ready".into());
            }
            if let Ok(identity) =
                LeaseGuardClient::discover(control.join("control.sock"), worker, None).await
            {
                break identity;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        };
        Ok(Self {
            client: LeaseGuardClient::bind(control.join("control.sock"), identity),
            process,
            control,
        })
    }
    pub async fn shutdown(&mut self) -> Result<()> {
        if self.process.try_wait()?.is_some() {
            return Err("owned guardian exited before shutdown".into());
        }
        // SAFETY: this parent owns the unreaped child, reserving its PID.
        if unsafe { libc::kill(self.process.id() as i32, libc::SIGTERM) } != 0 {
            return Err("guardian shutdown signal failed".into());
        }
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Some(status) = self.process.try_wait()? {
                return if status.success() {
                    Ok(())
                } else {
                    Err("guardian shutdown failed".into())
                };
            }
            if Instant::now() > deadline {
                return Err("guardian could not confirm writer shutdown".into());
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }
}
impl Drop for Guard {
    fn drop(&mut self) {
        let _ = self.process.kill();
        let _ = self.process.wait();
        let _ = fs::remove_dir_all(&self.control);
    }
}
