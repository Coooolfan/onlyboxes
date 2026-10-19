use std::collections::BTreeMap;

use super::HostProcess;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub const ARCHIVE_FORMAT: &str = "boxlite-archive-v1";

#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum Error {
    #[error("snapshot operation or generation conflict")]
    Conflict,
    #[error("snapshot journal is unavailable")]
    Unavailable,
}

type Result<T> = std::result::Result<T, Error>;

// Deliberately omit Debug: startup environment and commands can contain secrets.
#[derive(Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RestoreSpec {
    pub runtime: String,
    pub runtime_version: String,
    pub platform: String,
    pub format: String,
    pub image: String,
    pub memory_mib: i64,
    pub cpus: f64,
    pub max_processes: i32,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub user: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub working_dir: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub env: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub entrypoint: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub command: Vec<String>,
}

impl RestoreSpec {
    pub fn validate(&self) -> Result<()> {
        let Some((name, digest)) = self.image.rsplit_once("@sha256:") else {
            return Err(Error::Conflict);
        };
        if self.runtime != "boxlite"
            || self.runtime_version.trim().is_empty()
            || self.runtime_version.len() > 128
            || !matches!(self.platform.as_str(), "linux/amd64" | "linux/arm64")
            || self.format != ARCHIVE_FORMAT
            || name.is_empty()
            || self
                .image
                .bytes()
                .any(|b| b.is_ascii_whitespace() || b == 0)
            || !valid_digest(digest)
            || self.memory_mib <= 0
            || self.memory_mib > u32::MAX as i64
            || !self.cpus.is_finite()
            || self.cpus <= 0.0
            || self.cpus > u8::MAX as f64
            || self.cpus.fract() != 0.0
            || self.max_processes <= 0
            || serde_json::to_vec(self).map_err(|_| Error::Conflict)?.len() > 32 << 10
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }
}

#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Archive {
    pub snapshot_id: String,
    pub size_bytes: i64,
    pub sha256: String,
    pub expanded_bytes: i64,
    pub format: String,
}

impl Archive {
    pub fn validate(&self) -> Result<()> {
        if !valid_id(&self.snapshot_id)
            || self.size_bytes <= 0
            || self.size_bytes > 1 << 60
            || self.expanded_bytes <= 0
            || self.expanded_bytes > 1 << 60
            || !valid_digest(&self.sha256)
            || self.format != ARCHIVE_FORMAT
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Kind {
    Source,
    Target,
    Adopt,
    Cancel,
    Delete,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    Preparing,
    Frozen,
    Committed,
    Finalized,
    RollingBack,
    RolledBack,
    Prepared,
    Importing,
    Ready,
    Active,
    Canceling,
    Canceled,
    Deleting,
    Deleted,
}

#[derive(Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Operation {
    pub operation_id: String,
    pub scoped_session_id: String,
    pub generation: i64,
    pub kind: Kind,
    pub phase: Phase,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub restore_spec: Option<RestoreSpec>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub archive: Option<Archive>,
    #[serde(default)]
    pub artifact: String,
    #[serde(default)]
    pub box_id: String,
    #[serde(default)]
    pub tombstone_box_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source_process: Option<HostProcess>,
    #[serde(default)]
    pub source_stop_confirmed: bool,
    /// Durable intent precedes a controlled native boot. An uncertain stopped
    /// attempt cannot be retried by assuming that the old writer disappeared.
    #[serde(default)]
    pub start_attempted: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub started_process: Option<HostProcess>,
    #[serde(default)]
    pub lease_expires_unix_ms: i64,
    #[serde(default)]
    pub staging_bytes: i64,
    #[serde(default)]
    pub snapshot_committed: bool,
    #[serde(default)]
    pub verified: bool,
    #[serde(default)]
    pub remote_verified: bool,
    #[serde(default)]
    pub cache_until_unix_ms: i64,
    #[serde(default)]
    pub lease_ttl_sec: i32,
}

impl Operation {
    pub fn active(&self) -> bool {
        matches!(self.phase, Phase::Active | Phase::RolledBack)
    }
    pub fn terminal(&self) -> bool {
        matches!(
            self.phase,
            Phase::Finalized | Phase::Canceled | Phase::Deleted
        )
    }
    pub fn held(&self) -> bool {
        !self.active() && !self.terminal()
    }

    pub fn validate(&self) -> Result<()> {
        if !valid_id(&self.operation_id)
            || !valid_scope(&self.scoped_session_id)
            || self.generation <= 0
            || self.staging_bytes < 0
            || self.staging_bytes > 1 << 60
            || self.lease_expires_unix_ms < 0
            || self.lease_ttl_sec < 0
            || self.cache_until_unix_ms < 0
            || (!self.box_id.is_empty() && !valid_id(&self.box_id))
            || (!self.tombstone_box_id.is_empty() && !valid_id(&self.tombstone_box_id))
        {
            return Err(Error::Conflict);
        }
        if let Some(process) = &self.started_process {
            process.validate()?;
        }
        if (self.start_attempted
            && (!matches!(self.kind, Kind::Source | Kind::Target)
                || self.box_id.is_empty()
                || self.lease_expires_unix_ms == 0
                || !matches!(
                    self.phase,
                    Phase::Importing
                        | Phase::Ready
                        | Phase::Active
                        | Phase::RollingBack
                        | Phase::RolledBack
                        | Phase::Canceling
                        | Phase::Canceled
                )))
            || (self.started_process.is_some() && !self.start_attempted)
        {
            return Err(Error::Conflict);
        }
        if let Some(process) = &self.source_process {
            process.validate()?;
        }
        if (self.kind != Kind::Source
            && (self.source_process.is_some() || self.source_stop_confirmed))
            || (self.source_stop_confirmed && self.source_process.is_none())
            || (self.kind == Kind::Source
                && matches!(
                    self.phase,
                    Phase::Frozen | Phase::Committed | Phase::Finalized
                )
                && !self.source_stop_confirmed)
        {
            return Err(Error::Conflict);
        }
        if self.kind == Kind::Cancel {
            return if self.phase == Phase::Canceled
                && self.restore_spec.is_none()
                && self.archive.is_none()
                && self.artifact.is_empty()
                && self.box_id.is_empty()
                && self.tombstone_box_id.is_empty()
                && self.staging_bytes == 0
                && self.lease_expires_unix_ms == 0
                && self.lease_ttl_sec == 0
                && self.cache_until_unix_ms == 0
                && !self.verified
                && !self.remote_verified
                && !self.snapshot_committed
            {
                Ok(())
            } else {
                Err(Error::Conflict)
            };
        }
        self.restore_spec
            .as_ref()
            .ok_or(Error::Conflict)?
            .validate()?;
        if let Some(archive) = &self.archive {
            archive.validate()?;
            if self.kind == Kind::Source && archive.snapshot_id != self.operation_id {
                return Err(Error::Conflict);
            }
        }
        let valid_phase = match self.kind {
            Kind::Source => matches!(
                self.phase,
                Phase::Preparing
                    | Phase::Frozen
                    | Phase::Committed
                    | Phase::Finalized
                    | Phase::RollingBack
                    | Phase::RolledBack
            ),
            Kind::Target => matches!(
                self.phase,
                Phase::Prepared
                    | Phase::Importing
                    | Phase::Ready
                    | Phase::Active
                    | Phase::Canceling
                    | Phase::Canceled
            ),
            Kind::Adopt => self.phase == Phase::Active,
            Kind::Delete => matches!(self.phase, Phase::Deleting | Phase::Deleted),
            Kind::Cancel => false,
        };
        if !valid_phase
            || (!self.artifact.is_empty()
                && self.artifact
                    != artifact_name(&self.scoped_session_id, &self.operation_id, self.generation))
            || (self.remote_verified
                && (self.kind != Kind::Source || !self.verified || self.archive.is_none()))
            || (self.verified && self.archive.is_none())
            || (self.snapshot_committed && (self.kind != Kind::Source || !self.remote_verified))
            || (matches!(self.phase, Phase::Committed | Phase::Finalized)
                != self.snapshot_committed)
            || ((self.kind == Kind::Source || self.active() || self.phase == Phase::Ready)
                && self.box_id.is_empty())
            || ((self.active() || self.phase == Phase::Ready) && self.lease_expires_unix_ms <= 0)
            || (self.kind == Kind::Target
                && (self.archive.is_none() || self.staging_bytes <= 0 || self.artifact.is_empty()))
            || (self.kind == Kind::Target
                && matches!(self.phase, Phase::Importing | Phase::Ready | Phase::Active)
                && !self.verified)
            || (matches!(
                self.phase,
                Phase::Frozen | Phase::Committed | Phase::Finalized
            ) && (self.archive.is_none() || self.artifact.is_empty() || !self.verified))
            || (self.kind != Kind::Delete && !self.tombstone_box_id.is_empty())
            || (self.kind == Kind::Adopt
                && (self.archive.is_some() || !self.artifact.is_empty() || self.staging_bytes != 0))
            || (self.kind == Kind::Delete
                && (self.generation <= 1
                    || self.archive.is_some()
                    || !self.artifact.is_empty()
                    || self.staging_bytes != 0
                    || self.lease_expires_unix_ms != 0
                    || self.lease_ttl_sec != 0
                    || self.cache_until_unix_ms != 0
                    || self.verified
                    || self.remote_verified
                    || self.snapshot_committed
                    || (self.phase == Phase::Deleted && self.tombstone_box_id.is_empty())))
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }

    pub(crate) fn initial(&self) -> bool {
        matches!(
            (self.kind, self.phase),
            (Kind::Source, Phase::Preparing)
                | (Kind::Target, Phase::Prepared)
                | (Kind::Adopt, Phase::Active)
                | (Kind::Delete, Phase::Deleting)
        )
    }

    pub(crate) fn can_update_to(&self, next: &Self) -> bool {
        self.operation_id == next.operation_id
            && self.scoped_session_id == next.scoped_session_id
            && self.generation == next.generation
            && self.kind == next.kind
            && self.restore_spec == next.restore_spec
            && (self.source_process.is_none() || self.source_process == next.source_process)
            && (!self.source_stop_confirmed || next.source_stop_confirmed)
            && (!self.start_attempted || next.start_attempted)
            && (self.started_process.is_none() || self.started_process == next.started_process)
            && self.staging_bytes == next.staging_bytes
            && self.lease_ttl_sec == next.lease_ttl_sec
            && (self.box_id.is_empty() || self.box_id == next.box_id)
            && (self.tombstone_box_id.is_empty() || self.tombstone_box_id == next.tombstone_box_id)
            && (self.artifact.is_empty() || self.artifact == next.artifact)
            && (self.archive.is_none() || self.archive == next.archive)
            && (!self.verified || next.verified)
            && (!self.remote_verified || next.remote_verified)
            && (!self.snapshot_committed || next.snapshot_committed)
            && self.lease_expires_unix_ms <= next.lease_expires_unix_ms
            && phase_transition(self.kind, self.phase, next.phase)
    }
}

fn phase_transition(kind: Kind, from: Phase, to: Phase) -> bool {
    if from == to {
        return true;
    }
    match kind {
        Kind::Source => matches!(
            (from, to),
            (Phase::Preparing, Phase::Frozen | Phase::RollingBack)
                | (Phase::Frozen, Phase::Committed | Phase::RollingBack)
                | (Phase::Committed, Phase::Finalized)
                | (Phase::RollingBack, Phase::RolledBack)
        ),
        Kind::Target => {
            matches!(
                (from, to),
                (Phase::Prepared, Phase::Importing)
                    | (Phase::Importing, Phase::Ready)
                    | (Phase::Ready, Phase::Active)
                    | (Phase::Canceling, Phase::Canceled)
            ) || (from != Phase::Canceled && to == Phase::Canceling)
        }
        Kind::Delete => from == Phase::Deleting && to == Phase::Deleted,
        Kind::Adopt | Kind::Cancel => false,
    }
}

pub fn artifact_name(scope: &str, operation: &str, generation: i64) -> String {
    let identity =
        serde_json::to_vec(&(scope, operation, generation)).expect("string tuple serializes");
    format!("{:x}.boxlite", Sha256::digest(identity))
}

pub(crate) fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}
fn valid_digest(digest: &str) -> bool {
    digest.len() == 64
        && digest
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
pub(crate) fn valid_scope(scope: &str) -> bool {
    let parts: Vec<_> = scope.splitn(3, ':').collect();
    scope.len() <= 1024
        && parts.len() == 3
        && parts[0] == "obx"
        && !parts[1].is_empty()
        && !parts[2].is_empty()
        && scope.trim() == scope
        && !scope.bytes().any(|b| matches!(b, 0 | b'\r' | b'\n'))
}
