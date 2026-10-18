package sandboxsnapshot

import (
	"context"
	"database/sql"
)

// WorkerRecoveryPlan reads ordinary running ownership and lifecycle-held
// resources in one database snapshot. Separate reads could omit a target as it
// moves from activating to running and expose it to worker orphan cleanup.
type WorkerRecoveryPlan struct {
	RequiresPersistence bool
	PersistentScopes    map[string]bool
	Running             []RecoveryRunning
	Held                []RecoveryHeld
}

type RecoveryRunning struct {
	ScopedSessionID    string
	Generation         int64
	LeaseExpiresUnixMS int64
}

type RecoveryHeld struct {
	ScopedSessionID   string
	OperationID       string
	Generation        int64
	Source            bool
	SnapshotCommitted bool
	RouteCommitted    bool
}

func (r *Repository) WorkerRecovery(ctx context.Context, worker string) (WorkerRecoveryPlan, error) {
	plan := WorkerRecoveryPlan{PersistentScopes: make(map[string]bool)}
	err := r.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT s.scoped_session_id,s.state,s.generation,s.operation_id,s.worker_id,
		COALESCE(o.source_worker_id,''),COALESCE(o.target_worker_id,''),COALESCE(o.status,''),COALESCE(o.phase,''),
		COALESCE(r.node_id,''),COALESCE(r.lease_expires_unix_ms,0)
		FROM sandbox_persistent_sessions s
		LEFT JOIN sandbox_persistence_operations o ON o.operation_id=s.operation_id
		LEFT JOIN terminal_session_routes r ON r.scoped_session_id=s.scoped_session_id
		ORDER BY s.scoped_session_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var scope, state, op, owner, source, target, status, phase, routeWorker string
			var generation, lease int64
			if err := rows.Scan(&scope, &state, &generation, &op, &owner, &source, &target, &status, &phase, &routeWorker, &lease); err != nil {
				return err
			}
			plan.PersistentScopes[scope] = true
			if owner == worker || source == worker || target == worker {
				plan.RequiresPersistence = true
			}
			if op == "" {
				if state == string(Running) && owner == worker && routeWorker == worker && lease > r.now().UnixMilli() {
					plan.Running = append(plan.Running, RecoveryRunning{scope, generation, lease})
				}
				continue
			}
			if status != "pending" && status != "running" && !(status == "failed" && phase == "reconciling") {
				return ErrConflict
			}
			if source == worker {
				plan.Held = append(plan.Held, RecoveryHeld{ScopedSessionID: scope, OperationID: op, Generation: generation, Source: true, SnapshotCommitted: state == string(Suspended)})
			}
			if target == worker {
				plan.Held = append(plan.Held, RecoveryHeld{ScopedSessionID: scope, OperationID: op, Generation: generation, RouteCommitted: state == string(Running)})
			}
		}
		return rows.Err()
	})
	return plan, err
}
