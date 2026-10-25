//! The private disk encoding shares immutable specifications without removing
//! operation, native, boot or retirement evidence from the logical journal.
use super::*;
use serde::de::Error as _;
use sha2::{Digest, Sha256};
use std::collections::BTreeSet;

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct StoredState {
    version: u32,
    worker_id: String,
    current: BTreeMap<String, String>,
    operations: BTreeMap<String, Operation>,
    probes: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    adoptions: BTreeMap<String, super::super::InitialAdoption>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    initial_boots: BTreeMap<String, super::super::InitialBoot>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    initial_creations: BTreeMap<String, super::super::InitialCreation>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    restore_specs: BTreeMap<String, super::super::RestoreSpec>,
}

// Counts encoded bytes without allocating an expanded multi-megabyte buffer.
struct BoundedCount(usize);
impl Write for BoundedCount {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        self.0 = self
            .0
            .checked_add(bytes.len())
            .filter(|size| *size <= MAX_JOURNAL_BYTES)
            .ok_or_else(|| std::io::Error::other("snapshot journal size limit"))?;
        Ok(bytes.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

fn encoded_size(value: &impl Serialize) -> Result<usize> {
    let mut count = BoundedCount(0);
    serde_json::to_writer(&mut count, value).map_err(|_| Error::Unavailable)?;
    Ok(count.0)
}

fn spec_digest(spec: &super::super::RestoreSpec) -> Result<String> {
    let bytes = serde_json::to_vec(spec).map_err(|_| Error::Unavailable)?;
    Ok(format!("{:x}", Sha256::digest(bytes)))
}

impl StoredState {
    /// Bound expansion before cloning a shared specification into operations.
    /// The encoded pool/reference bytes plus every referenced specification is
    /// a conservative upper bound on the hydrated JSON size. Both encoder and
    /// decoder apply the same limit, so every successful write can be reopened.
    fn check_expansion(&self) -> Result<()> {
        let mut total = encoded_size(self)?;
        let mut sizes = BTreeMap::new();
        for (digest, spec) in &self.restore_specs {
            spec.validate().map_err(|_| Error::Unavailable)?;
            if *digest != spec_digest(spec)? {
                return Err(Error::Unavailable);
            }
            sizes.insert(digest, encoded_size(spec)?);
        }
        let mut referenced = BTreeSet::new();
        for op in self.operations.values() {
            if op.restore_spec.is_some() {
                return Err(Error::Unavailable);
            }
            if let Some(digest) = &op.restore_spec_sha256 {
                let size = sizes.get(digest).ok_or(Error::Unavailable)?;
                total = total
                    .checked_add(*size)
                    .filter(|size| *size <= MAX_JOURNAL_BYTES)
                    .ok_or(Error::Unavailable)?;
                referenced.insert(digest);
            }
        }
        if referenced.len() != self.restore_specs.len() {
            return Err(Error::Unavailable);
        }
        Ok(())
    }

    fn hydrate(mut self) -> Result<JournalState> {
        match self.version {
            1 => {
                if !self.restore_specs.is_empty()
                    || self
                        .operations
                        .values()
                        .any(|op| op.restore_spec_sha256.is_some())
                {
                    return Err(Error::Unavailable);
                }
            }
            2 => {
                self.check_expansion()?;
                for op in self.operations.values_mut() {
                    if let Some(digest) = op.restore_spec_sha256.take() {
                        op.restore_spec = Some(
                            self.restore_specs
                                .get(&digest)
                                .ok_or(Error::Unavailable)?
                                .clone(),
                        );
                    }
                    op.validate().map_err(|_| Error::Unavailable)?;
                }
            }
            _ => return Err(Error::Unavailable),
        }
        Ok(JournalState {
            version: 1,
            worker_id: self.worker_id,
            current: self.current,
            operations: self.operations,
            probes: self.probes,
            adoptions: self.adoptions,
            initial_boots: self.initial_boots,
            initial_creations: self.initial_creations,
        })
    }
}

// Logical snapshots always expose full validated specifications. Raw disk v2
// also deserializes through this path in native fixtures and home preflight.
impl<'de> Deserialize<'de> for JournalState {
    fn deserialize<D: serde::Deserializer<'de>>(decoder: D) -> std::result::Result<Self, D::Error> {
        StoredState::deserialize(decoder)?
            .hydrate()
            .map_err(|_| D::Error::custom("snapshot journal encoding is invalid"))
    }
}

pub(super) fn encode(state: &JournalState) -> Result<Vec<u8>> {
    if state.version != 1 {
        return Err(Error::Unavailable);
    }
    let mut stored = StoredState {
        version: 2,
        worker_id: state.worker_id.clone(),
        current: state.current.clone(),
        operations: BTreeMap::new(),
        probes: state.probes.clone(),
        adoptions: state.adoptions.clone(),
        initial_boots: state.initial_boots.clone(),
        initial_creations: state.initial_creations.clone(),
        restore_specs: BTreeMap::new(),
    };
    for (id, op) in &state.operations {
        op.validate().map_err(|_| Error::Unavailable)?;
        let mut op = op.clone();
        if let Some(spec) = op.restore_spec.take() {
            let digest = spec_digest(&spec)?;
            if stored
                .restore_specs
                .get(&digest)
                .is_some_and(|existing| *existing != spec)
            {
                return Err(Error::Unavailable);
            }
            stored.restore_specs.insert(digest.clone(), spec);
            op.restore_spec_sha256 = Some(digest);
        }
        stored.operations.insert(id.clone(), op);
    }
    stored.check_expansion()?;
    serde_json::to_vec(&stored).map_err(|_| Error::Unavailable)
}
