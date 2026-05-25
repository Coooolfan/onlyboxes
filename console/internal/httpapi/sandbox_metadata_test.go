package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func TestSandboxMetadataAllowsJITToken(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	jitToken := makeTestJITToken(t, "issuer-metadata", "subject-metadata")
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_000, 0)
	seedWorkerForSandboxMetadata(t, store, "node-terminal", now, "", registry.WorkerTypeNormal, []registry.CapabilityDeclaration{
		{Name: "terminalExec", MaxInflight: 4},
		{Name: "terminalResource", MaxInflight: 2},
	})
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return message, nil
		},
	}, nil, nil, "")
	handler.nowFn = func() time.Time { return now }
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), auth, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandbox/metadata", nil)
	req.Header.Set(trustedTokenHeader, "Bearer "+jitToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var body sandboxMetadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Provider != sandboxProviderName {
		t.Fatalf("expected provider %q, got %q", sandboxProviderName, body.Provider)
	}
	if !metadataCapabilityAvailable(body.Capabilities, "terminalExec") {
		t.Fatalf("expected terminalExec capability to be available: %#v", body.Capabilities)
	}
	if !metadataCapabilityAvailable(body.Capabilities, "terminalResource") {
		t.Fatalf("expected terminalResource capability to be available: %#v", body.Capabilities)
	}
	if metadataCapabilityAvailable(body.Capabilities, "computerUse") {
		t.Fatalf("did not expect computerUse to be available without caller-owned worker-sys")
	}
}

func TestSandboxMetadataScopesWorkerSysCapabilitiesByJITAccount(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	jitToken := makeTestJITToken(t, "issuer-metadata", "subject-owned")
	identity, ok := deriveJITAccountIdentity(jitTokenClaims{
		Issuer:  "issuer-metadata",
		Subject: "subject-owned",
	})
	if !ok {
		t.Fatalf("expected jit identity")
	}
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_000, 0)
	seedWorkerForSandboxMetadata(t, store, "node-sys-owned", now, identity.AccountID, registry.WorkerTypeSys, []registry.CapabilityDeclaration{
		{Name: "computerUse", MaxInflight: 1},
		{Name: "readImage", MaxInflight: 1},
	})
	seedWorkerForSandboxMetadata(t, store, "node-sys-other", now, "acc-other", registry.WorkerTypeSys, []registry.CapabilityDeclaration{
		{Name: "computerUse", MaxInflight: 1},
	})
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return message, nil
		},
	}, nil, nil, "")
	handler.nowFn = func() time.Time { return now }
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), auth, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandbox/metadata", nil)
	req.Header.Set(trustedTokenHeader, "Bearer "+jitToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var body sandboxMetadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !metadataCapabilityAvailable(body.Capabilities, "computerUse") {
		t.Fatalf("expected caller-owned computerUse to be available: %#v", body.Capabilities)
	}
	if got := metadataCapabilityOnlineNodes(body.Capabilities, "computerUse"); got != 1 {
		t.Fatalf("expected one scoped computerUse node, got %d", got)
	}
	if !metadataCapabilityAvailable(body.Capabilities, "readImage") {
		t.Fatalf("expected caller-owned readImage to be available: %#v", body.Capabilities)
	}
}

func TestSandboxMetadataRequiresExecutionToken(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return message, nil
		},
	}, nil, nil, "")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandbox/metadata", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func seedWorkerForSandboxMetadata(t *testing.T, store *registry.Store, nodeID string, now time.Time, ownerID string, workerType string, capabilities []registry.CapabilityDeclaration) {
	t.Helper()

	labels := map[string]string{}
	if ownerID != "" {
		labels[registry.LabelOwnerIDKey] = ownerID
	}
	if workerType != "" {
		labels[registry.LabelWorkerTypeKey] = workerType
	}

	protoCapabilities := make([]*registryv1.CapabilityDeclaration, 0, len(capabilities))
	for _, capability := range capabilities {
		protoCapabilities = append(protoCapabilities, &registryv1.CapabilityDeclaration{
			Name:        capability.Name,
			MaxInflight: capability.MaxInflight,
		})
	}

	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId:       nodeID,
		NodeName:     nodeID,
		ExecutorKind: "test",
		Capabilities: protoCapabilities,
		Labels:       labels,
		Version:      "test",
	}, "session-"+nodeID, now); err != nil {
		t.Fatalf("seed worker %s: %v", nodeID, err)
	}
}

func metadataCapabilityAvailable(capabilities []sandboxMetadataCapability, name string) bool {
	for _, capability := range capabilities {
		if capability.Name == name {
			return capability.Available
		}
	}
	return false
}

func metadataCapabilityOnlineNodes(capabilities []sandboxMetadataCapability, name string) int {
	for _, capability := range capabilities {
		if capability.Name == name {
			return capability.OnlineNodes
		}
	}
	return 0
}
