use super::*;
use std::process::{Child, Command, Stdio};

struct Sleeper(Child);
impl Sleeper {
    fn new() -> Self {
        Self(
            Command::new("sleep")
                .arg("300")
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .unwrap(),
        )
    }
}
impl Drop for Sleeper {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
#[test]
fn identity_is_stable_and_serializable_within_one_host_boot() {
    let first = HostProcess::capture(std::process::id()).unwrap();
    let second = HostProcess::capture(std::process::id()).unwrap();
    assert_eq!(first, second);
    assert_eq!(first.has_exited(), Ok(false));
    let encoded = serde_json::to_vec(&first).unwrap();
    assert_eq!(
        serde_json::from_slice::<HostProcess>(&encoded).unwrap(),
        first
    );
}
#[test]
fn a_real_child_is_not_exited_until_it_terminates() {
    let mut child = Sleeper::new();
    let process = HostProcess::capture(child.0.id()).unwrap();
    assert_eq!(process.has_exited(), Ok(false));
    child.0.kill().unwrap();
    child.0.wait().unwrap();
    assert_eq!(process.has_exited(), Ok(true));
}

#[test]
fn a_stop_request_never_falls_back_to_an_unpinned_pid() {
    let mut child = Sleeper::new();
    let process = HostProcess::capture(child.0.id()).unwrap();
    assert_eq!(process.request_writer_stop(), Err(Error::Unavailable));
    assert!(child.0.try_wait().unwrap().is_none());
}

#[cfg(target_os = "macos")]
#[test]
fn exact_stop_refuses_foreign_host_namespace_and_process_versions() {
    let mut child = Sleeper::new();
    let mut process = HostProcess::capture(child.0.id()).unwrap();
    process.bind_writer_group("owned_fixture").unwrap();
    let mut foreign = process.clone();
    foreign.host_id = uuid::Uuid::new_v4().to_string();
    assert!(foreign.request_writer_stop().is_err());
    let mut moved = process.clone();
    moved.process_namespace = "linux-pid:1:2".into();
    assert!(moved.request_writer_stop().is_err());
    let mut wrong = process.clone();
    wrong.signal_version = process
        .signal_version
        .map(|version| version.wrapping_add(1));
    assert!(wrong.request_writer_stop().is_err());
    assert!(child.0.try_wait().unwrap().is_none());
    process.request_writer_stop().unwrap();
    child.0.wait().unwrap();
    assert_eq!(process.writers_exited(), Ok(true));
    assert_eq!(process.request_writer_stop(), Ok(()));
}

#[test]
fn launcher_only_evidence_cannot_prove_linux_writers_exited() {
    let mut child = Sleeper::new();
    let mut process = HostProcess::capture(child.0.id()).unwrap();
    assert_eq!(process.writers_exited(), Ok(false));
    child.0.kill().unwrap();
    child.0.wait().unwrap();
    assert_eq!(process.has_exited(), Ok(true));
    #[cfg(target_os = "linux")]
    assert_eq!(process.writers_exited(), Err(Error::Unavailable));
    #[cfg(target_os = "macos")]
    assert_eq!(process.writers_exited(), Ok(true));
    process.boot_id = uuid::Uuid::new_v4().to_string();
    assert_eq!(process.writers_exited(), Ok(true));
}
#[test]
fn pid_reuse_and_reboot_do_not_signal_the_current_process() {
    let process = HostProcess::capture(std::process::id()).unwrap();
    let mut reused = process.clone();
    reused.start_time += 1;
    assert_eq!(reused.has_exited(), Ok(true));
    let mut rebooted = process.clone();
    rebooted.boot_id = uuid::Uuid::new_v4().to_string();
    assert_eq!(rebooted.has_exited(), Ok(true));
    assert_eq!(process.has_exited(), Ok(false));
}
#[test]
fn missing_os_evidence_is_never_a_proof_of_exit() {
    let process = HostProcess::capture(std::process::id()).unwrap();
    assert_eq!(
        classify(
            &process,
            &process.host_id,
            &process.boot_id,
            &process.process_namespace,
            Observation::Unavailable
        ),
        Err(Error::Unavailable)
    );
    assert_eq!(
        classify(
            &process,
            &process.host_id,
            &process.boot_id,
            &process.process_namespace,
            Observation::Present(0)
        ),
        Err(Error::Unavailable)
    );
    assert_eq!(
        classify(
            &process,
            &process.host_id,
            &process.boot_id,
            &process.process_namespace,
            Observation::Present(process.start_time)
        ),
        Ok(false)
    );
    assert_eq!(
        classify(
            &process,
            &process.host_id,
            &process.boot_id,
            &process.process_namespace,
            Observation::Present(process.start_time + 1)
        ),
        Ok(true)
    );
    assert_eq!(
        classify(
            &process,
            &process.host_id,
            &process.boot_id,
            &process.process_namespace,
            Observation::Gone
        ),
        Ok(true)
    );
}
#[test]
fn malformed_or_special_pid_records_are_rejected_before_os_queries() {
    let process = HostProcess::capture(std::process::id()).unwrap();
    for pid in [0, 1, u32::MAX, i32::MAX as u32 + 1] {
        let mut bad = process.clone();
        bad.pid = pid;
        assert_eq!(bad.has_exited(), Err(Error::Conflict));
        assert_eq!(HostProcess::capture(pid), Err(Error::Conflict));
    }
    for boot in ["", "not-a-uuid", "00000000-0000-0000-0000-000000000000"] {
        let mut bad = process.clone();
        bad.boot_id = boot.into();
        assert_eq!(bad.has_exited(), Err(Error::Conflict));
    }
    for namespace in ["", "linux-pid:0:0", "linux-pid:01:2", "linux-pid:1:-1"] {
        let mut bad = process.clone();
        bad.process_namespace = namespace.into();
        assert_eq!(bad.has_exited(), Err(Error::Conflict));
    }
    let mut bad = process;
    bad.start_time = 0;
    assert_eq!(bad.has_exited(), Err(Error::Conflict));
}

#[test]
fn moving_process_evidence_to_another_host_does_not_prove_exit() {
    let process = HostProcess::capture(std::process::id()).unwrap();
    let mut foreign = process.clone();
    foreign.host_id = uuid::Uuid::new_v4().to_string();
    foreign.boot_id = uuid::Uuid::new_v4().to_string();
    assert_eq!(foreign.has_exited(), Err(Error::Unavailable));
    assert_eq!(
        classify(
            &process,
            &foreign.host_id,
            &foreign.boot_id,
            &foreign.process_namespace,
            Observation::Gone
        ),
        Err(Error::Unavailable)
    );
}

#[test]
fn changing_pid_namespace_cannot_make_an_unobserved_writer_disappear() {
    let process = HostProcess::capture(std::process::id()).unwrap();
    let mut moved = process.clone();
    moved.process_namespace = "linux-pid:1:2".into();
    assert_ne!(moved.process_namespace, process.process_namespace);
    assert_eq!(moved.has_exited(), Err(Error::Unavailable));
    assert_eq!(
        classify(
            &moved,
            &process.host_id,
            &process.boot_id,
            &process.process_namespace,
            Observation::Gone
        ),
        Err(Error::Unavailable)
    );
}

#[cfg(any(target_os = "macos", target_os = "linux"))]
#[test]
fn an_unreaped_terminated_child_is_positive_exit_evidence() {
    let mut child = Sleeper::new();
    let process = HostProcess::capture(child.0.id()).unwrap();
    #[cfg(target_os = "macos")]
    let process = {
        let mut process = process;
        process.bind_writer_group("unreaped_fixture").unwrap();
        process
    };
    assert_eq!(process.has_exited(), Ok(false));
    child.0.kill().unwrap();
    let mut info = std::mem::MaybeUninit::<libc::siginfo_t>::zeroed();
    // SAFETY: waitid observes only this unreaped owned child. WNOWAIT retains
    // its PID and lets the Child owner perform the eventual reap below.
    assert_eq!(
        unsafe {
            libc::waitid(
                libc::P_PID,
                child.0.id(),
                info.as_mut_ptr(),
                libc::WEXITED | libc::WNOWAIT,
            )
        },
        0
    );
    assert_eq!(process.has_exited(), Ok(true));
    #[cfg(target_os = "macos")]
    assert_eq!(process.writers_exited(), Ok(true));
    #[cfg(target_os = "linux")]
    assert_eq!(process.writers_exited(), Err(Error::Unavailable));
    child.0.wait().unwrap();
}
