package grpcserver

import (
	"context"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func (s *RegistryService) snapshotRecoveryPlan(ctx context.Context, worker string, ordinary []*registryv1.TerminalSessionRecoveryCandidate) ([]*registryv1.TerminalSessionRecoveryCandidate, []*registryv1.SandboxPersistenceRecoveryCandidate, bool, error) {
	if s.snapshotRepository == nil {
		return ordinary, nil, false, nil
	}
	plan, err := s.snapshotRepository.WorkerRecovery(ctx, worker)
	if err != nil {
		return nil, nil, false, err
	}
	filtered := make([]*registryv1.TerminalSessionRecoveryCandidate, 0, len(ordinary)+len(plan.Running))
	for _, candidate := range ordinary {
		if !plan.PersistentScopes[candidate.GetSessionId()] {
			filtered = append(filtered, candidate)
		}
	}
	for _, running := range plan.Running {
		filtered = append(filtered, &registryv1.TerminalSessionRecoveryCandidate{SessionId: running.ScopedSessionID, Generation: running.Generation, LeaseExpiresUnixMs: running.LeaseExpiresUnixMS})
	}
	held := make([]*registryv1.SandboxPersistenceRecoveryCandidate, 0, len(plan.Held))
	for _, resource := range plan.Held {
		role := registryv1.SandboxPersistenceRecoveryCandidate_TARGET
		if resource.Source {
			role = registryv1.SandboxPersistenceRecoveryCandidate_SOURCE
		}
		held = append(held, &registryv1.SandboxPersistenceRecoveryCandidate{ScopedSessionId: resource.ScopedSessionID, OperationId: resource.OperationID, Generation: resource.Generation, Role: role, SnapshotCommitted: resource.SnapshotCommitted, RouteCommitted: resource.RouteCommitted})
	}
	return filtered, held, plan.RequiresPersistence, nil
}
