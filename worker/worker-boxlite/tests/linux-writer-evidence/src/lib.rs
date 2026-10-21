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
