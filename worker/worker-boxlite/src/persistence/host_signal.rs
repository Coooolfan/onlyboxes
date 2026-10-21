//! Kernel-validated process versions avoid check-then-kill PID reuse races.
#[cfg(target_os = "macos")]
use super::Error;

#[cfg(target_os = "macos")]
#[repr(C)]
struct AuditToken {
    values: [u32; 8],
}

#[cfg(target_os = "macos")]
unsafe extern "C" {
    static mach_task_self_: u32;
    fn task_name_for_pid(task: u32, pid: i32, name: *mut u32) -> i32;
    fn mach_port_deallocate(task: u32, name: u32) -> i32;
    fn proc_signal_with_audittoken(token: *const AuditToken, signal: i32) -> i32;
}

#[cfg(target_os = "macos")]
pub(super) fn version(pid: u32) -> Result<u32, Error> {
    let mut port = 0;
    // SAFETY: all output pointers reference valid stack storage. Only a task
    // name right is requested; no task-control or debugger access is used.
    let task = unsafe { mach_task_self_ };
    if unsafe { task_name_for_pid(task, pid as i32, &mut port) } != 0 || port == 0 {
        return Err(Error::Unavailable);
    }
    let mut token = AuditToken { values: [0; 8] };
    let mut words = 8;
    // TASK_AUDIT_TOKEN is the public SDK flavor 15; the token is eight u32s.
    // SAFETY: token has exactly the writable capacity supplied in words.
    let result = unsafe { libc::task_info(port, 15, token.values.as_mut_ptr().cast(), &mut words) };
    // SAFETY: release only the task-name right acquired above.
    unsafe {
        mach_port_deallocate(task, port);
    }
    if result != 0 || words != 8 || token.values[5] != pid || token.values[7] == 0 {
        return Err(Error::Unavailable);
    }
    Ok(token.values[7])
}

#[cfg(target_os = "macos")]
pub(super) fn kill(pid: u32, version: u32) -> Result<(), Error> {
    if pid <= 1 || pid > i32::MAX as u32 || version == 0 {
        return Err(Error::Conflict);
    }
    let mut token = AuditToken { values: [0; 8] };
    token.values[5] = pid;
    token.values[7] = version;
    // SAFETY: the fixed-size audit token pins PID + version inside the kernel.
    // Permissions are checked against the calling process, not token UID fields.
    // SIGKILL has no guest signal handler; this never targets a process group.
    if unsafe { proc_signal_with_audittoken(&token, libc::SIGKILL) } != 0 {
        return Err(Error::Unavailable);
    }
    Ok(())
}

#[cfg(all(test, target_os = "macos"))]
mod tests {
    use super::*;
    #[test]
    fn a_wrong_kernel_version_cannot_signal_an_owned_live_process() {
        let mut child = std::process::Command::new("sleep")
            .arg("30")
            .spawn()
            .unwrap();
        let id = version(child.id()).unwrap();
        assert!(kill(child.id(), id.wrapping_add(1)).is_err());
        assert!(child.try_wait().unwrap().is_none());
        kill(child.id(), id).unwrap();
        let status = child.wait().unwrap();
        use std::os::unix::process::ExitStatusExt;
        assert_eq!(status.signal(), Some(libc::SIGKILL));
        assert!(kill(child.id(), id).is_err());
    }
}
