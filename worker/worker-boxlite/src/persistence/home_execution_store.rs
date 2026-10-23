//! A dedicated SDK home retains the external guard store identity across epochs.
use super::*;
use crate::persistence::ExecutionLeaseLedger;
const STORE: &str = "execution-store.json";
#[derive(PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct StoreBinding {
    pair_id: String,
    store_id: String,
    state: Directory,
}
impl RuntimeHomeGuard {
    fn execution_store_binding(&self, state: &Path, store_id: &str) -> Result<StoreBinding> {
        self.verify(&self.owner.home.path, &self.owner.worker_id)?;
        ExecutionLeaseLedger::verify_external_store(state, &self.owner.worker_id, store_id)?;
        let state = Directory::inspect(state)?;
        if state.path.starts_with(&self.owner.home.path)
            || self.owner.home.path.starts_with(&state.path)
            || state.path.starts_with(&self.owner.journal.path)
            || self.owner.journal.path.starts_with(&state.path)
        {
            return Err(Error::Unavailable);
        }
        Ok(StoreBinding {
            pair_id: self.owner.pair_id.clone(),
            store_id: store_id.into(),
            state,
        })
    }
    /// Pair only an empty journal, or verify the exact previously retained store.
    /// Missing bindings around existing session history are never recreated.
    pub fn bind_execution_store(&self, state: &Path, store_id: &str) -> Result<()> {
        let expected = self.execution_store_binding(state, store_id)?;
        match read_private(&self.owner.journal.path.join(STORE), 8192)? {
            Some(bytes) => {
                let saved: StoreBinding =
                    serde_json::from_slice(&bytes).map_err(|_| Error::Unavailable)?;
                if saved != expected {
                    return Err(Error::Unavailable);
                }
            }
            None => {
                let journal = self.journal.snapshot()?;
                if !journal.operations.is_empty()
                    || !journal.probes.is_empty()
                    || !journal.initial_boots.is_empty()
                    || !journal.adoptions.is_empty()
                {
                    return Err(Error::Unavailable);
                }
                write_atomic(
                    &self.owner.journal.path,
                    STORE,
                    &serde_json::to_vec(&expected).map_err(|_| Error::Unavailable)?,
                )?;
            }
        }
        self.verify_execution_store(state, store_id)
    }
    pub fn verify_execution_store(&self, state: &Path, store_id: &str) -> Result<()> {
        let expected = self.execution_store_binding(state, store_id)?;
        let saved: StoreBinding = serde_json::from_slice(
            &read_private(&self.owner.journal.path.join(STORE), 8192)?.ok_or(Error::Unavailable)?,
        )
        .map_err(|_| Error::Unavailable)?;
        if saved != expected {
            return Err(Error::Unavailable);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn live_external_store_is_read_only_and_home_pairing_cannot_change_or_reset() {
        let root =
            std::env::temp_dir().join(format!("onlyboxes-store-pair-{}", uuid::Uuid::new_v4()));
        private_directory(&root).unwrap();
        let home = root.join("home");
        let journal = root.join("journal");
        let state = root.join("leases");
        let first = ExecutionLeaseLedger::open(&state, "worker-a").unwrap();
        let id = first.store_id().to_owned();
        // Verification must not compete for the already-held service lock.
        ExecutionLeaseLedger::verify_external_store(&state, "worker-a", &id).unwrap();
        assert!(ExecutionLeaseLedger::verify_external_store(&state, "worker-b", &id).is_err());
        let owner = RuntimeHomeGuard::open(&home, &journal, "worker-a").unwrap();
        owner.bind_execution_store(&state, &id).unwrap();
        owner.bind_execution_store(&state, &id).unwrap();
        let second = ExecutionLeaseLedger::open(root.join("other-leases"), "worker-a").unwrap();
        assert!(owner
            .bind_execution_store(&root.join("other-leases"), second.store_id())
            .is_err());
        let copy = root.join("copy");
        private_directory(&copy).unwrap();
        fs::copy(state.join("owner.json"), copy.join("owner.json")).unwrap();
        assert!(ExecutionLeaseLedger::verify_external_store(&copy, "worker-a", &id).is_err());
        drop(owner);
        let owner = RuntimeHomeGuard::open(&home, &journal, "worker-a").unwrap();
        owner.verify_execution_store(&state, &id).unwrap();
        owner
            .journal()
            .probe("obx:owner:session", "native-box")
            .unwrap();
        fs::remove_file(journal.join(STORE)).unwrap();
        assert!(owner.verify_execution_store(&state, &id).is_err());
        assert!(owner.bind_execution_store(&state, &id).is_err());
        drop(owner);
        drop(first);
        drop(second);
        fs::remove_dir_all(root).unwrap();
    }
}
