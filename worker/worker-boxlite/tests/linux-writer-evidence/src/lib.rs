//! Compile the production OS evidence module without the VM SDK or KVM.
//! This harness does not validate BoxLite's actual VM placement in cgroups.
#![allow(dead_code)]

#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    Conflict,
    Unavailable,
}

#[path = "../../../src/persistence/cgroup.rs"]
mod cgroup;

pub use cgroup::WriterCgroup;

// The SDK-free harness supplies only Box ID syntax at this boundary. All
// process birth, namespace, exit, zombie and subtree evidence is production code.
#[cfg(target_os = "linux")]
mod types {
    pub(crate) fn valid_id(id: &str) -> bool {
        !id.is_empty()
            && id.len() <= 128
            && id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
    }
}
#[cfg(target_os = "linux")]
#[path = "../../../src/persistence/host_process.rs"]
mod host_process;
