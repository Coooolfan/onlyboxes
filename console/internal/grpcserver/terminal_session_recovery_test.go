package grpcserver

import (
	"errors"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func TestTerminalRouteDisconnectRecoveryRoundTrip(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_100_000, 0)
	lease := base.Add(10 * time.Minute).UnixMilli()
	svc.nowFn = func() time.Time { return base.Add(time.Second) }

	svc.bindTerminalSessionRoute("session-a", "node-a", base)
	if !svc.updateTerminalSessionRouteLease("session-a", "node-a", lease, base) {
		t.Fatal("failed to store terminal lease")
	}
	if unavailable := svc.markTerminalSessionRoutesUnavailable("node-a"); unavailable != 1 {
		t.Fatalf("unavailable route count=%d, want 1", unavailable)
	}

	_, _, err := svc.pickSessionForDispatch(
		taskCapabilityTerminalExec,
		"owner-a",
		"session-a",
		sessionPickOptions{terminalSessionIntent: terminalSessionIntentKnownNew},
	)
	var commandErr *CommandExecutionError
	if !errors.As(err, &commandErr) || commandErr.Code != terminalSessionUnavailableCode {
		t.Fatalf("expected session_unavailable while disconnected, got %v", err)
	}

	candidates := svc.beginTerminalSessionRecovery("node-a", base.Add(time.Second))
	if len(candidates) != 1 || candidates[0].GetSessionId() != "session-a" || candidates[0].GetLeaseExpiresUnixMs() != lease {
		t.Fatalf("unexpected recovery candidates: %#v", candidates)
	}

	hello := &registryv1.ConnectHello{
		NodeId: "node-a",
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: taskCapabilityTerminalExec, MaxInflight: 1},
		},
	}
	session := newActiveSessionAt("node-a", "worker-session-a", hello, base.Add(time.Second))
	session.setRecoveryCandidates(candidates)
	err = svc.applyTerminalSessionRecoveryReport(session, &registryv1.TerminalSessionRecoveryReport{
		Results: []*registryv1.TerminalSessionRecoveryResult{
			{SessionId: "session-a", Status: registryv1.TerminalSessionRecoveryResult_RECOVERED},
		},
	}, base.Add(2*time.Second))
	if err != nil {
		t.Fatalf("apply recovery report: %v", err)
	}
	route, ok := svc.terminalSessionRouteSnapshot("session-a", base.Add(2*time.Second))
	if !ok || route.RecoveryState != terminalSessionRecoveryReady || route.LeaseExpiresUnixMs != lease {
		t.Fatalf("unexpected recovered route: %#v ok=%v", route, ok)
	}
}

func TestTerminalRecoveryMissingDeletesRoute(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_200_000, 0)
	svc.bindTerminalSessionRoute("session-missing", "node-a", base)
	svc.updateTerminalSessionRouteLease("session-missing", "node-a", base.Add(time.Minute).UnixMilli(), base)
	candidates := svc.beginTerminalSessionRecovery("node-a", base.Add(time.Second))
	session := newActiveSessionAt("node-a", "worker-session-a", &registryv1.ConnectHello{
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 1}},
	}, base)
	session.setRecoveryCandidates(candidates)

	err := svc.applyTerminalSessionRecoveryReport(session, &registryv1.TerminalSessionRecoveryReport{
		Results: []*registryv1.TerminalSessionRecoveryResult{
			{SessionId: "session-missing", Status: registryv1.TerminalSessionRecoveryResult_MISSING},
		},
	}, base.Add(2*time.Second))
	if err != nil {
		t.Fatalf("apply missing report: %v", err)
	}
	if _, ok := svc.terminalSessionRouteSnapshot("session-missing", base.Add(2*time.Second)); ok {
		t.Fatal("missing backend resource must delete route")
	}
}

func TestTerminalRecoveryUsesLegacyRouteTTLFallback(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.terminalRouteTTL = 30 * time.Minute
	base := time.Unix(1_700_300_000, 0)
	svc.bindTerminalSessionRoute("legacy-session", "node-a", base)

	candidates := svc.beginTerminalSessionRecovery("node-a", base.Add(time.Minute))
	wantLease := base.Add(30 * time.Minute).UnixMilli()
	if len(candidates) != 1 || candidates[0].GetLeaseExpiresUnixMs() != wantLease {
		t.Fatalf("legacy route candidate lease=%v, want %d", candidates, wantLease)
	}
}

func TestTerminalRecoveryDropsRouteWhoseLeaseExpiredOffline(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_350_000, 0)
	svc.bindTerminalSessionRoute("expired-session", "node-a", base)
	svc.updateTerminalSessionRouteLease("expired-session", "node-a", base.Add(time.Minute).UnixMilli(), base)
	svc.markTerminalSessionRoutesUnavailable("node-a")
	if candidates := svc.beginTerminalSessionRecovery("node-a", base.Add(2*time.Minute)); len(candidates) != 0 {
		t.Fatalf("expired route was offered for recovery: %#v", candidates)
	}
	if _, ok := svc.terminalSessionRouteSnapshot("expired-session", base.Add(2*time.Minute)); ok {
		t.Fatal("expired offline route was not deleted")
	}
}

func TestPruneUsesExactLeaseWhenLegacyTTLDisabled(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.terminalRouteTTL = 0
	base := time.Unix(1_700_375_000, 0)
	svc.bindTerminalSessionRoute("expired-session", "node-a", base)
	svc.updateTerminalSessionRouteLease("expired-session", "node-a", base.Add(time.Minute).UnixMilli(), base)
	if removed := svc.pruneExpiredTerminalSessionRoutes(base.Add(2 * time.Minute)); removed != 1 {
		t.Fatalf("removed=%d, want exact-lease route pruned", removed)
	}
}

func TestTerminalRecoveryReportMustCoverCandidates(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_400_000, 0)
	svc.bindTerminalSessionRoute("session-a", "node-a", base)
	svc.updateTerminalSessionRouteLease("session-a", "node-a", base.Add(time.Minute).UnixMilli(), base)
	candidates := svc.beginTerminalSessionRecovery("node-a", base)
	session := newActiveSessionAt("node-a", "worker-session-a", &registryv1.ConnectHello{
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 1}},
	}, base)
	session.setRecoveryCandidates(candidates)

	if err := svc.applyTerminalSessionRecoveryReport(session, &registryv1.TerminalSessionRecoveryReport{}, base); err == nil {
		t.Fatal("partial recovery report unexpectedly accepted")
	}
}

func TestTerminalRecoveryReportIsIdempotentOnlyWhenUnchanged(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_500_000, 0)
	svc.bindTerminalSessionRoute("session-a", "node-a", base)
	svc.updateTerminalSessionRouteLease("session-a", "node-a", base.Add(time.Minute).UnixMilli(), base)
	candidates := svc.beginTerminalSessionRecovery("node-a", base)
	session := newActiveSessionAt("node-a", "worker-session-a", &registryv1.ConnectHello{
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 1}},
	}, base)
	session.setRecoveryCandidates(candidates)
	report := &registryv1.TerminalSessionRecoveryReport{Results: []*registryv1.TerminalSessionRecoveryResult{
		{SessionId: "session-a", Status: registryv1.TerminalSessionRecoveryResult_RECOVERED},
	}}
	if err := svc.applyTerminalSessionRecoveryReport(session, report, base.Add(time.Second)); err != nil {
		t.Fatalf("apply recovery report: %v", err)
	}
	session.markRecoveryComplete()
	if !session.matchesRecoveryResults(report) {
		t.Fatal("unchanged recovery report must be accepted as an idempotent retry")
	}
	changed := &registryv1.TerminalSessionRecoveryReport{Results: []*registryv1.TerminalSessionRecoveryResult{
		{SessionId: "session-a", Status: registryv1.TerminalSessionRecoveryResult_MISSING},
	}}
	if session.matchesRecoveryResults(changed) {
		t.Fatal("changed recovery report must not be accepted as an idempotent retry")
	}
}

func TestTerminalRecoveryRejectsCandidateReassignedToAnotherWorker(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_600_000, 0)
	svc.bindTerminalSessionRoute("session-a", "node-a", base)
	svc.updateTerminalSessionRouteLease("session-a", "node-a", base.Add(time.Minute).UnixMilli(), base)
	candidates := svc.beginTerminalSessionRecovery("node-a", base)
	session := newActiveSessionAt("node-a", "worker-session-a", &registryv1.ConnectHello{
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 1}},
	}, base)
	session.setRecoveryCandidates(candidates)
	svc.bindTerminalSessionRoute("session-a", "node-b", base.Add(time.Second))
	err := svc.applyTerminalSessionRecoveryReport(session, &registryv1.TerminalSessionRecoveryReport{
		Results: []*registryv1.TerminalSessionRecoveryResult{{
			SessionId: "session-a", Status: registryv1.TerminalSessionRecoveryResult_RECOVERED,
		}},
	}, base.Add(2*time.Second))
	if err == nil {
		t.Fatal("recovery report unexpectedly changed a route reassigned to another worker")
	}
	route, ok := svc.terminalSessionRouteSnapshot("session-a", base.Add(2*time.Second))
	if !ok || route.NodeID != "node-b" {
		t.Fatalf("reassigned route changed: %#v ok=%v", route, ok)
	}
}
