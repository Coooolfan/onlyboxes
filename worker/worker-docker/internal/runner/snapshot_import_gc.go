package runner

import (
	"context"
	"sort"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/logging"
)

type snapshotRetiredRoot struct {
	scope      string
	generation int64
	container  string
}

// A finalized source proves physical removal and independent lease release,
// not merely expiry, suspension intent or the existence of a newer operation.
func finalizedSnapshotRoots(state snapshotJournalState) map[snapshotRetiredRoot]snapshotOperation {
	retired := make(map[snapshotRetiredRoot]snapshotOperation)
	for _, op := range state.Operations {
		if op.Kind == "source" && op.Phase == "finalized" && op.Committed && op.RemoteVerified && op.Verified && op.Archive != nil && op.ContainerID != "" {
			retired[snapshotRetiredRoot{op.SessionID, op.Generation, op.ContainerID}] = op
		}
	}
	return retired
}

func snapshotImportRetired(state snapshotJournalState, roots map[snapshotRetiredRoot]snapshotOperation, op snapshotOperation) bool {
	if op.Kind != "target" || op.Archive == nil {
		return false
	}
	if op.Phase == "canceled" {
		return true
	}
	current := state.Operations[state.Current[op.SessionID]]
	if current.ID == op.ID {
		return false
	}
	if current.Kind == "delete" && current.Phase == "deleted" && current.SessionID == op.SessionID && current.Generation > op.Generation {
		return true
	}
	proof, found := roots[snapshotRetiredRoot{op.SessionID, op.Generation, op.ContainerID}]
	return op.ContainerID != "" && found && snapshotSpecEqual(proof.Spec, op.Spec)
}

// Retry even after successful cleanup: an ambiguously acknowledged import may
// finish late. The durable operation/tag identity stays in the journal. Only
// metadata-proven retired imports qualify; this is never a global image prune.
func (c *snapshotController) reconcileRetiredImports(ctx context.Context, state snapshotJournalState, now time.Time, next map[string]time.Time) int {
	roots := finalizedSnapshotRoots(state)
	var candidates []snapshotOperation
	for _, op := range state.Operations {
		if !now.Before(next[op.ID]) && snapshotImportRetired(state, roots, op) {
			candidates = append(candidates, op)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !next[a.ID].Equal(next[b.ID]) {
			return next[a.ID].Before(next[b.ID])
		}
		return a.ID < b.ID
	})
	launched := 0
	for _, op := range candidates {
		if launched >= c.config.MaxInflight {
			break
		}
		run, cancel := context.WithTimeout(ctx, 10*time.Second)
		c.mu.Lock()
		if ctx.Err() != nil || c.closed || c.recovering || c.actions[op.SessionID].cancel != nil || len(c.actions) >= c.config.MaxInflight {
			c.mu.Unlock()
			cancel()
			continue
		}
		c.actions[op.SessionID] = snapshotActiveAction{operation: op.ID, generation: op.Generation, cancel: cancel}
		c.wg.Add(1)
		c.mu.Unlock()
		next[op.ID] = now.Add(time.Minute)
		launched++
		go func(op snapshotOperation) {
			defer cancel()
			defer func() { c.mu.Lock(); delete(c.actions, op.SessionID); c.mu.Unlock(); c.wg.Done() }()
			// Admission owns the scope before this fresh read, so a concurrent
			// restore/cancel/recovery cannot change the proof while cleanup runs.
			fresh, err := c.journal.Snapshot()
			if err != nil {
				return
			}
			current, found := fresh.Operations[op.ID]
			if !found || !snapshotImportRetired(fresh, finalizedSnapshotRoots(fresh), current) {
				return
			}
			if err := c.removeImportTag(run, current); err != nil && run.Err() == nil {
				logging.Warnf("snapshot retired import cleanup remains pending: operation_id=%s", op.ID)
			}
		}(op)
	}
	return launched
}
