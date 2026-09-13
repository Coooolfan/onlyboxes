package grpcserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSubmitTaskComputerUseWithoutWorkerReturnsOwnedWorkerList(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_100_000, 0)
	workers := []registry.ProvisionedWorker{
		{NodeID: "node-online", Labels: map[string]string{registry.LabelOwnerIDKey: "owner-a", registry.LabelWorkerTypeKey: registry.WorkerTypeSys}},
		{NodeID: "node-offline", Labels: map[string]string{registry.LabelOwnerIDKey: "owner-a", registry.LabelWorkerTypeKey: registry.WorkerTypeSys}},
		{NodeID: "node-other", Labels: map[string]string{registry.LabelOwnerIDKey: "owner-b", registry.LabelWorkerTypeKey: registry.WorkerTypeSys}},
	}
	if seeded := store.SeedProvisionedWorkers(workers, now, 15*time.Second); seeded != len(workers) {
		t.Fatalf("seeded=%d, want %d", seeded, len(workers))
	}
	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId: "node-online", NodeName: "desktop", Labels: workers[0].Labels,
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: computerUseCapabilityDeclared, MaxInflight: 2}},
	}, "session-online", now); err != nil {
		t.Fatalf("mark online worker: %v", err)
	}

	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	result, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "computerUse", InputJSON: []byte(`{"command":"must not run"}`),
		Mode: TaskModeAsync, OwnerID: "owner-a",
	})
	if err != nil {
		t.Fatalf("submit list task: %v", err)
	}
	if !result.Completed || result.Task.Status != TaskStatusSucceeded || result.Task.CommandID != "" {
		t.Fatalf("unexpected list task: %#v", result)
	}
	var decoded workerSysListResult
	if err := json.Unmarshal(result.Task.ResultJSON, &decoded); err != nil {
		t.Fatalf("decode worker list: %v", err)
	}
	if len(decoded.WorkerList) != 2 {
		t.Fatalf("worker_list=%#v", decoded.WorkerList)
	}
	statuses := map[string]registry.WorkerStatus{}
	for _, item := range decoded.WorkerList {
		statuses[item.WorkerID] = item.Status
	}
	if statuses["node-online"] != registry.StatusOnline || statuses["node-offline"] != registry.StatusOffline {
		t.Fatalf("unexpected statuses: %#v", statuses)
	}
	if len(decoded.WorkerList[1].Capabilities) == 0 && len(decoded.WorkerList[0].Capabilities) == 0 {
		t.Fatalf("online worker capabilities were not included: %#v", decoded.WorkerList)
	}
	if _, exists := statuses["node-other"]; exists {
		t.Fatalf("foreign worker leaked into list")
	}
}

func TestPrepareTaskInputRoutesWorkerSysSessions(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_100_100, 0)
	store.SeedProvisionedWorkers([]registry.ProvisionedWorker{{
		NodeID: "node-a", Labels: map[string]string{registry.LabelOwnerIDKey: "owner-a", registry.LabelWorkerTypeKey: registry.WorkerTypeSys},
	}}, now, 15*time.Second)
	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }

	prepared, err := svc.prepareTaskInput(readImageCapabilityName, "owner-a", []byte(`{"session_id":"CU:node-a","file_path":"/tmp/a.png"}`))
	if err != nil {
		t.Fatalf("prepare readImage: %v", err)
	}
	if prepared.targetNodeID != "node-a" || prepared.dispatchCapability != readImageCapabilityName {
		t.Fatalf("unexpected prepared target: %#v", prepared)
	}
	if !strings.Contains(string(prepared.inputJSON), `"session_id":"computerUse"`) || strings.Contains(string(prepared.inputJSON), "node-a") {
		t.Fatalf("worker payload was not normalized: %s", prepared.inputJSON)
	}

	legacy, err := svc.prepareTaskInput(readImageCapabilityName, "owner-a", []byte(`{"session_id":"computerUse","file_path":"/tmp/a.png"}`))
	if err != nil || legacy.targetNodeID != "" || legacy.dispatchCapability != taskCapabilityTerminalResource {
		t.Fatalf("legacy value must be a sandbox session: prepared=%#v err=%v", legacy, err)
	}
	lowercasePrefix, err := svc.prepareTaskInput(readImageCapabilityName, "owner-a", []byte(`{"session_id":"cu:node-a","file_path":"/tmp/a.png"}`))
	if err != nil || lowercasePrefix.targetNodeID != "" || lowercasePrefix.dispatchCapability != taskCapabilityTerminalResource {
		t.Fatalf("prefix matching must be case-sensitive: prepared=%#v err=%v", lowercasePrefix, err)
	}
	_, err = svc.prepareTaskInput(readImageCapabilityName, "owner-a", []byte(`{"session_id":"CU:  ","file_path":"/tmp/a.png"}`))
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "worker_id") {
		t.Fatalf("expected empty prefixed worker ID rejection, got %v", err)
	}

	svc.SetComputerUseSessionIDPrefix("SYS/")
	custom, err := svc.prepareTaskInput(readImageCapabilityName, "owner-a", []byte(`{"session_id":"SYS/node-a","file_path":"/tmp/a.png"}`))
	if err != nil || custom.targetNodeID != "node-a" {
		t.Fatalf("custom prefix was not routed: prepared=%#v err=%v", custom, err)
	}
}

func TestPrepareTaskInputRejectsReservedTerminalSessionCreation(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	_, err := svc.prepareTaskInput(taskCapabilityTerminalExec, "owner-a", []byte(`{"command":"pwd","session_id":"CU:node-a","create_if_missing":true}`))
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), `"CU:"`) {
		t.Fatalf("expected reserved-prefix invalid argument, got %v", err)
	}
	prepared, err := svc.prepareTaskInput(taskCapabilityTerminalExec, "owner-a", []byte(`{"command":"pwd","session_id":"CU:node-a","create_if_missing":false}`))
	if err != nil || prepared.targetNodeID != "" {
		t.Fatalf("lookup-only terminal session should remain a sandbox request: %#v err=%v", prepared, err)
	}
}

func TestPrepareTaskInputHidesForeignWorkerExistence(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_100_200, 0)
	store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{NodeID: "node-b", Labels: map[string]string{registry.LabelOwnerIDKey: "owner-b", registry.LabelWorkerTypeKey: registry.WorkerTypeSys}},
		{NodeID: "node-normal", Labels: map[string]string{registry.LabelOwnerIDKey: "owner-a", registry.LabelWorkerTypeKey: registry.WorkerTypeNormal}},
	}, now, 15*time.Second)
	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	for _, workerID := range []string{"node-b", "node-normal", "node-unknown"} {
		_, err := svc.prepareTaskInput(computerUseCapabilityName, "owner-a", []byte(`{"worker_id":"`+workerID+`","command":"pwd"}`))
		if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "worker_id is invalid" {
			t.Fatalf("unexpected error for hidden target %q: %v", workerID, err)
		}
	}
}

func TestTargetWorkerAvailabilityErrorsRemainDistinct(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_100_300, 0)
	store.SeedProvisionedWorkers([]registry.ProvisionedWorker{{
		NodeID: "node-offline", Labels: map[string]string{registry.LabelOwnerIDKey: "owner-a", registry.LabelWorkerTypeKey: registry.WorkerTypeSys},
	}}, now, 15*time.Second)
	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	if err := svc.checkTargetCapabilityAvailability("node-offline", computerUseCapabilityName); err != ErrTargetWorkerOffline {
		t.Fatalf("expected offline error, got %v", err)
	}

	svc.sessionsMu.Lock()
	svc.sessions["node-offline"] = newActiveSession("node-offline", "session-online", &registryv1.ConnectHello{
		NodeId: "node-offline", Capabilities: []*registryv1.CapabilityDeclaration{{Name: "echo", MaxInflight: 1}},
	})
	svc.sessionsMu.Unlock()
	if err := svc.checkTargetCapabilityAvailability("node-offline", computerUseCapabilityName); err != ErrTargetWorkerCapabilityUnavailable {
		t.Fatalf("expected capability-unavailable error, got %v", err)
	}
}
