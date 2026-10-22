use super::*;
use std::os::unix::fs::{symlink, PermissionsExt};

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        let root = std::env::temp_dir().join(format!(
            "onlyboxes-execution-lease-{}",
            uuid::Uuid::new_v4()
        ));
        private_directory(&root).unwrap();
        Self(root)
    }
    fn open(&self) -> ExecutionLeaseLedger {
        ExecutionLeaseLedger::open(self.0.join("ledger"), "worker").unwrap()
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

fn clock_lease() -> ExecutionLease {
    ExecutionLease {
        box_id: "owned_box".into(),
        scoped_session_id: "obx:owner:session".into(),
        generation: 1,
        revision: 1,
        expires_unix_ms: Moment::now().unwrap().wall + 60_000,
        process: HostProcess::capture(std::process::id()).unwrap(),
        revoked: false,
    }
}

#[test]
fn wall_clock_rollback_cannot_extend_or_revive_an_expired_watch() {
    let mut lease = clock_lease();
    let base = Moment::now().unwrap();
    lease.expires_unix_ms = base.wall + 1000;
    let mut watch = Watch::initial(&lease, base, false).unwrap();
    assert!(watch.live(Moment {
        wall: base.wall - 100_000,
        monotonic: base.monotonic + Duration::from_millis(500)
    }));
    let renewed = ExecutionLease {
        expires_unix_ms: base.wall + 2000,
        revision: 2,
        ..lease.clone()
    };
    let mut next = watch.renewed(&renewed).unwrap();
    assert_eq!(next.deadline, base.monotonic + Duration::from_secs(2));
    assert!(!watch.live(Moment {
        wall: base.wall - 100_000,
        monotonic: base.monotonic + Duration::from_secs(1)
    }));
    assert!(!watch.live(base));
    assert!(!next.live(Moment {
        wall: base.wall - 100_000,
        monotonic: base.monotonic + Duration::from_secs(2)
    }));
    assert!(!next.live(base));
}

#[test]
fn ledger_is_private_exclusive_bound_and_never_resets_missing_history() {
    let root = Root::new();
    let ledger = root.open();
    let id = ledger.store_id().to_owned();
    assert!(ExecutionLeaseLedger::open(&ledger.root, "worker").is_err());
    for name in ["owner.json", "leases.json", "guard.lock"] {
        assert_eq!(
            fs::metadata(ledger.root.join(name))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
    }
    let path = ledger.root.clone();
    drop(ledger);
    assert!(ExecutionLeaseLedger::open(&path, "other-worker").is_err());
    assert_eq!(root.open().store_id(), id);
    let copy = root.0.join("copy");
    private_directory(&copy).unwrap();
    for name in ["owner.json", "leases.json"] {
        fs::copy(path.join(name), copy.join(name)).unwrap();
    }
    assert!(ExecutionLeaseLedger::open(&copy, "worker").is_err());
    fs::remove_file(path.join("leases.json")).unwrap();
    assert!(ExecutionLeaseLedger::open(&path, "worker").is_err());
    assert!(!path.join("leases.json").exists());
}

#[test]
fn pinned_store_identity_refuses_missing_or_reinitialized_ledger_roots() {
    let root = Root::new();
    let ledger = root.open();
    let id = ledger.store_id().to_owned();
    let path = ledger.root.clone();
    drop(ledger);
    assert!(ExecutionLeaseLedger::open_bound(&path, "worker", &id).is_ok());
    assert!(
        ExecutionLeaseLedger::open_bound(&path, "worker", &uuid::Uuid::new_v4().to_string())
            .is_err()
    );
    fs::rename(&path, root.0.join("old")).unwrap();
    assert!(ExecutionLeaseLedger::open_bound(&path, "worker", &id).is_err());
    assert!(!path.exists());
    private_directory(&path).unwrap();
    assert!(ExecutionLeaseLedger::open_bound(&path, "worker", &id).is_err());
    assert!(!path.join("owner.json").exists());
    assert!(!path.join("leases.json").exists());
}

#[test]
fn tampered_state_and_redirected_private_files_stay_unavailable() {
    let root = Root::new();
    let ledger = root.open();
    let path = ledger.root.clone();
    drop(ledger);
    let bytes = fs::read(path.join("leases.json")).unwrap();
    let mut state: State = serde_json::from_slice(&bytes).unwrap();
    state.store_id = uuid::Uuid::new_v4().to_string();
    write_atomic(&path, "leases.json", &serde_json::to_vec(&state).unwrap()).unwrap();
    assert!(ExecutionLeaseLedger::open(&path, "worker").is_err());
    write_atomic(&path, "leases.json", &bytes).unwrap();
    fs::rename(path.join("leases.json"), root.0.join("outside")).unwrap();
    symlink(root.0.join("outside"), path.join("leases.json")).unwrap();
    assert!(ExecutionLeaseLedger::open(&path, "worker").is_err());
}

#[test]
fn an_open_handle_cannot_follow_replaced_directories_or_lock_files() {
    let root = Root::new();
    let ledger = root.open();
    fs::rename(&ledger.root, root.0.join("old-ledger")).unwrap();
    private_directory(&ledger.root).unwrap();
    assert_eq!(ledger.status("missing"), Err(Error::Unavailable));
    assert!(!ledger.root.join("owner.json").exists());
    drop(ledger);
    fs::remove_dir(root.0.join("ledger")).unwrap();
    fs::rename(root.0.join("old-ledger"), root.0.join("ledger")).unwrap();
    let ledger = root.open();
    fs::remove_file(ledger.root.join("guard.lock")).unwrap();
    fs::write(ledger.root.join("guard.lock"), b"").unwrap();
    assert_eq!(ledger.status("missing"), Err(Error::Unavailable));
}

#[cfg(target_os = "macos")]
mod native {
    use super::*;
    struct Child(std::process::Child);
    impl Child {
        fn new() -> Self {
            Self(
                std::process::Command::new("sleep")
                    .arg("30")
                    .spawn()
                    .unwrap(),
            )
        }
        fn lease(&self, ttl: i64) -> ExecutionLease {
            let mut lease = clock_lease();
            lease.process = HostProcess::capture(self.0.id()).unwrap();
            lease.process.bind_writer_group(&lease.box_id).unwrap();
            lease.expires_unix_ms = Moment::now().unwrap().wall + ttl;
            lease
        }
    }
    impl Drop for Child {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }

    #[test]
    fn missing_renewal_and_already_expired_adoption_do_not_create_ownership() {
        let root = Root::new();
        let mut child = Child::new();
        let ledger = root.open();
        let lease = child.lease(10_000);
        assert_eq!(
            ledger.adopt(ExecutionLease {
                generation: 2,
                ..lease.clone()
            }),
            Err(Error::Conflict)
        );
        assert_eq!(
            ledger.renew(
                1,
                ExecutionLease {
                    revision: 2,
                    ..lease.clone()
                }
            ),
            Err(Error::Conflict)
        );
        let expired = ExecutionLease {
            expires_unix_ms: Moment::now().unwrap().wall - 1,
            ..lease.clone()
        };
        assert_eq!(ledger.adopt(expired), Err(Error::Conflict));
        assert_eq!(ledger.status(&lease.box_id), Ok(None));
        assert_eq!(ledger.enforce(), Ok(LeaseEnforcement::default()));
        assert!(child.0.try_wait().unwrap().is_none());
    }

    #[test]
    fn competing_renewals_can_confirm_only_one_receipt_at_the_same_revision() {
        let root = Root::new();
        let child = Child::new();
        let ledger = Arc::new(root.open());
        let first = child.lease(10_000);
        ledger.adopt(first.clone()).unwrap();
        let start = Arc::new(std::sync::Barrier::new(3));
        let mut threads = Vec::new();
        for delta in [1000, 2000] {
            let ledger = ledger.clone();
            let start = start.clone();
            let next = ExecutionLease {
                revision: 2,
                expires_unix_ms: first.expires_unix_ms + delta,
                ..first.clone()
            };
            threads.push(std::thread::spawn(move || {
                start.wait();
                ledger.renew(1, next)
            }));
        }
        start.wait();
        let results: Vec<_> = threads
            .into_iter()
            .map(|thread| thread.join().unwrap())
            .collect();
        assert_eq!(results.iter().filter(|result| result.is_ok()).count(), 1);
        assert!(results.contains(&Err(Error::Conflict)));
        let confirmed = results.into_iter().find_map(|result| result.ok()).unwrap();
        assert_eq!(ledger.status(&first.box_id), Ok(Some(confirmed)));
        assert_eq!(ledger.enforce(), Ok(LeaseEnforcement::default()));
    }

    #[test]
    fn a_committed_renewal_replay_does_not_confirm_a_disappeared_writer() {
        let root = Root::new();
        let mut child = Child::new();
        let ledger = root.open();
        let first = child.lease(10_000);
        ledger.adopt(first.clone()).unwrap();
        let next = ExecutionLease {
            revision: 2,
            expires_unix_ms: first.expires_unix_ms + 1000,
            ..first
        };
        ledger.renew(1, next.clone()).unwrap();
        child.0.kill().unwrap();
        child.0.wait().unwrap();
        assert!(ledger.renew(1, next.clone()).is_err());
        assert_eq!(ledger.status(&next.box_id), Ok(Some(next)));
    }

    #[test]
    fn receipts_are_replayable_but_stale_or_changed_owners_are_not() {
        let root = Root::new();
        let child = Child::new();
        let ledger = root.open();
        let first = child.lease(10_000);
        assert_eq!(ledger.adopt(first.clone()).unwrap(), first);
        assert_eq!(ledger.adopt(first.clone()).unwrap(), first);
        let next = ExecutionLease {
            revision: 2,
            expires_unix_ms: first.expires_unix_ms + 1000,
            ..first.clone()
        };
        assert_eq!(ledger.renew(1, next.clone()).unwrap(), next);
        assert_eq!(ledger.renew(1, next.clone()).unwrap(), next);
        assert_eq!(ledger.status(&first.box_id).unwrap(), Some(next.clone()));
        let duplicate = ExecutionLease {
            box_id: "other_box".into(),
            ..first.clone()
        };
        assert!(ledger.adopt(duplicate).is_err());
        for change in ["generation", "scope", "process", "deadline"] {
            let mut changed = ExecutionLease {
                revision: 3,
                ..next.clone()
            };
            match change {
                "generation" => changed.generation += 1,
                "scope" => changed.scoped_session_id = "obx:owner:other".into(),
                "process" => changed.process.start_time += 1,
                _ => changed.expires_unix_ms -= 1,
            }
            assert!(ledger.renew(2, changed).is_err(), "{change}");
        }
        assert!(ledger.renew(0, first.clone()).is_err());
        assert_eq!(ledger.status(&first.box_id).unwrap(), Some(next));
    }

    #[test]
    fn restart_revokes_historical_grants_without_losing_their_exact_stop_targets() {
        let root = Root::new();
        let mut child = Child::new();
        let ledger = root.open();
        let lease = child.lease(10_000);
        ledger.adopt(lease.clone()).unwrap();
        let store_id = ledger.store_id().to_owned();
        drop(ledger);
        let reopened = root.open();
        assert_eq!(reopened.store_id(), store_id);
        assert!(reopened.status(&lease.box_id).unwrap().unwrap().revoked);
        assert!(reopened.adopt(lease.clone()).is_err());
        let next = ExecutionLease {
            revision: 2,
            expires_unix_ms: lease.expires_unix_ms + 1000,
            ..lease
        };
        assert!(reopened.renew(1, next).is_err());
        assert_eq!(
            reopened.enforce().unwrap(),
            LeaseEnforcement {
                attempted: 1,
                failed: 0
            }
        );
        child.0.wait().unwrap();
    }

    #[test]
    fn uncertain_retirement_write_reopens_as_reserved_exit_evidence() {
        let root = Root::new();
        let mut child = Child::new();
        let mut ledger = root.open();
        let lease = child.lease(10_000);
        ledger.adopt(lease.clone()).unwrap();
        child.0.kill().unwrap();
        child.0.wait().unwrap();
        let retirement = crate::persistence::LeaseRetirement {
            operation_id: "cleanup_1".into(),
            cleanup_generation: 1,
            box_id: lease.box_id.clone(),
            scoped_session_id: lease.scoped_session_id,
            generation: 1,
            revision: lease.revision,
            process: lease.process,
        };
        ledger.persist = persisted_but_unconfirmed;
        assert_eq!(
            ledger.prepare_retirement(retirement.clone()),
            Err(Error::Unavailable)
        );
        assert_eq!(ledger.enforce().unwrap().attempted, 0);
        let store = ledger.store_id().to_owned();
        let path = ledger.root.clone();
        drop(ledger);
        let ledger = ExecutionLeaseLedger::open_bound(&path, "worker", &store).unwrap();
        assert!(
            !ledger
                .retirement_status(&retirement.box_id)
                .unwrap()
                .unwrap()
                .released
        );
        assert_eq!(
            ledger.prepare_retirement(retirement.clone()).unwrap(),
            retirement
        );
        assert!(ledger.status(&retirement.box_id).unwrap().unwrap().revoked);
        ledger.finish_retirement(retirement.clone()).unwrap();
        assert!(
            ledger
                .retirement_status(&retirement.box_id)
                .unwrap()
                .unwrap()
                .released
        );
    }

    fn persisted_but_unconfirmed(root: &Path, state: &State) -> Result<()> {
        persist_state(root, state)?;
        Err(Error::Unavailable)
    }
    #[test]
    fn uncertain_writes_close_admission_and_stop_even_the_unacknowledged_first_owner() {
        let root = Root::new();
        let mut child = Child::new();
        let mut ledger = root.open();
        ledger.persist = persisted_but_unconfirmed;
        let lease = child.lease(10_000);
        assert_eq!(ledger.adopt(lease.clone()), Err(Error::Unavailable));
        assert!(ledger.status(&lease.box_id).is_err());
        assert_eq!(
            ledger.enforce().unwrap(),
            LeaseEnforcement {
                attempted: 1,
                failed: 0
            }
        );
        child.0.wait().unwrap();
        drop(ledger);
        assert!(root.open().status(&lease.box_id).unwrap().unwrap().revoked);
    }

    fn delayed_write(root: &Path, state: &State) -> Result<()> {
        let parent = root.parent().unwrap();
        fs::write(parent.join("blocked"), b"").unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        while !parent.join("release").exists() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(5));
        }
        persist_state(root, state)
    }
    #[test]
    fn a_stalled_renewal_cannot_hold_the_old_deadline_or_revive_its_stopped_process() {
        let root = Root::new();
        let mut child = Child::new();
        let mut ledger = root.open();
        let lease = child.lease(400);
        ledger.adopt(lease.clone()).unwrap();
        ledger.persist = delayed_write;
        let ledger = Arc::new(ledger);
        let writer = ledger.clone();
        let next = ExecutionLease {
            revision: 2,
            expires_unix_ms: lease.expires_unix_ms + 10_000,
            ..lease.clone()
        };
        let operation = std::thread::spawn(move || writer.renew(1, next));
        let deadline = Instant::now() + Duration::from_secs(2);
        while !root.0.join("blocked").exists() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(root.0.join("blocked").exists());
        while child.0.try_wait().unwrap().is_none() && Instant::now() < deadline {
            ledger.enforce().unwrap();
            std::thread::sleep(Duration::from_millis(10));
        }
        let exited = child.0.try_wait().unwrap().is_some();
        fs::write(root.0.join("release"), b"").unwrap();
        assert_eq!(operation.join().unwrap(), Err(Error::Unavailable));
        assert!(exited, "expiry waited for the ledger mutation lock");
        assert!(ledger.status(&lease.box_id).unwrap().unwrap().revoked);
    }
}
