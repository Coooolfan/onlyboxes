package runner

import (
	"context"
	"errors"
	"os"
	"sort"
	"time"
)

func (c *snapshotController) copyCached(ctx context.Context, op snapshotOperation) error {
	if c.config.CacheMaxBytes == 0 {
		return os.ErrNotExist
	}
	state, err := c.journal.Snapshot()
	if err != nil {
		return err
	}
	for _, cached := range state.Operations {
		if cached.ID == op.ID || cached.SessionID != op.SessionID || !cached.Verified || cached.Archive == nil || *cached.Archive != *op.Archive || !snapshotSpecEqual(cached.Spec, op.Spec) || cached.CacheUntilUnixMS <= time.Now().UnixMilli() || (cached.Kind == "source" && !cached.Committed) {
			continue
		}
		file, release, err := c.openArtifact(cached.Artifact)
		if err != nil {
			continue
		}
		err = verifySnapshotFile(ctx, file, *op.Archive)
		if err == nil {
			c.mu.Lock()
			if removeErr := os.Remove(c.artifact(op.Artifact)); removeErr != nil && !os.IsNotExist(removeErr) {
				err = removeErr
			} else {
				err = os.Link(c.artifact(cached.Artifact), c.artifact(op.Artifact))
			}
			c.mu.Unlock()
		}
		release()
		if err == nil || errors.Is(err, os.ErrExist) {
			return nil
		}
	}
	return os.ErrNotExist
}

// Eviction affects only verified shared-snapshot copies. Pending sources,
// unverified files and files pinned by a transfer/import are never candidates.
func (c *snapshotController) evictCache() error { return c.evictCacheBudget(c.config.CacheMaxBytes) }

func (c *snapshotController) evictCacheBudget(budget int64) error {
	state, err := c.journal.Snapshot()
	if err != nil {
		return err
	}
	protected := map[string]bool{}
	for _, id := range state.Current {
		op := state.Operations[id]
		if op.held() {
			protected[op.Artifact] = true
		}
	}
	type entry struct {
		name     string
		size     int64
		modified time.Time
		expires  int64
	}
	var entries []entry
	seen := map[string]bool{}
	var total int64
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, op := range state.Operations {
		if op.Artifact == "" || seen[op.Artifact] || !op.Verified || (op.Kind == "source" && !op.Committed) {
			continue
		}
		seen[op.Artifact] = true
		info, err := os.Lstat(c.artifact(op.Artifact))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return errSnapshotJournalUnavailable
		}
		if protected[op.Artifact] || c.pins[op.Artifact] > 0 {
			continue
		}
		entries = append(entries, entry{op.Artifact, info.Size(), info.ModTime(), op.CacheUntilUnixMS})
		total += info.Size()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].modified.Before(entries[j].modified) })
	for _, entry := range entries {
		if entry.expires > time.Now().UnixMilli() && total <= budget {
			continue
		}
		if err := os.Remove(c.artifact(entry.name)); err != nil && !os.IsNotExist(err) {
			return errSnapshotJournalUnavailable
		}
		total -= entry.size
	}
	return nil
}
