# Linux writer subtree evidence

This small Cargo harness compiles the production `src/persistence/cgroup.rs` directly, without linking the BoxLite SDK or requiring KVM. The harness supplies only the two domain error variants used by that module. It does not replace the module with a simulated cgroup implementation.

```sh
cargo test --locked --manifest-path tests/linux-writer-evidence/Cargo.toml
BOXLITE_CGROUP_INTEGRATION=1 \
cargo test --locked --manifest-path tests/linux-writer-evidence/Cargo.toml -- --nocapture
```

The opt-in native test requires Linux with a writable cgroup v2 root at `/sys/fs/cgroup`, including `cgroup.kill`. It creates a randomly named `onlyboxes-writer-test-*` subtree, places an owned launcher and its child in it, reaps only the launcher, and checks that the remaining descendant prevents an empty-subtree acknowledgement. It then kills only that owned subtree, confirms it is empty, and rejects changed, missing and replaced cgroup identities. Cleanup removes only the fixture's own groups. Do not enable this test on a host where that delegation is unavailable.

The native test also runs in a temporary Linux container with the host cgroup namespace and writable cgroup v2 mount; compile the harness in a separate unprivileged build container, then run its test executable with network disabled. A container used for kernel tests needs privileges to create and kill its own temporary cgroups. The test does not start a Worker or access existing BoxLite homes.

This verifies Linux kernel evidence and descendant handling. It does not prove that a real BoxLite VM was placed in the expected cgroup, independent lease enforcement, or cross-machine snapshot recovery. Those require the full native SDK and Worker acceptance tests.
