package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func addTerminalCapacityTestWorker(
	t *testing.T,
	svc *RegistryService,
	nodeID string,
	now time.Time,
	capacity *registryv1.TerminalSessionCapacity,
) *activeSession {
	t.Helper()
	hello := &registryv1.ConnectHello{
		NodeId: nodeID,
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: taskCapabilityTerminalExec, MaxInflight: 4},
			{Name: taskCapabilityTerminalResource, MaxInflight: 4},
			{Name: echoCapabilityName, MaxInflight: 4},
		},
		TerminalSessionCapacity: capacity,
	}
	if err := svc.store.Upsert(hello, "worker-session-"+nodeID, now); err != nil {
		t.Fatalf("upsert worker %s: %v", nodeID, err)
	}
	session := newActiveSessionAt(nodeID, "worker-session-"+nodeID, hello, now)
	svc.swapSession(session)
	return session
}

func receiveCommandDispatch(t *testing.T, session *activeSession) *registryv1.CommandDispatch {
	t.Helper()
	select {
	case response := <-session.commandOutbound:
		dispatch := response.GetCommandDispatch()
		if dispatch == nil {
			t.Fatalf("expected command dispatch, got %#v", response.GetPayload())
		}
		return dispatch
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for command dispatch on %s", session.nodeID)
		return nil
	}
}

func assertNoCommandDispatch(t *testing.T, session *activeSession) {
	t.Helper()
	select {
	case response := <-session.commandOutbound:
		t.Fatalf("unexpected command dispatch on %s: %#v", session.nodeID, response.GetPayload())
	case <-time.After(50 * time.Millisecond):
	}
}

func terminalCapacity(maxActiveSessions int32, activeSessionCount int32) *registryv1.TerminalSessionCapacity {
	return &registryv1.TerminalSessionCapacity{
		MaxActiveSessions:  maxActiveSessions,
		ActiveSessionCount: activeSessionCount,
	}
}

func TestPickSessionForCapabilityUsesTerminalCapacityGroups(t *testing.T) {
	now := time.Unix(1_700_100_000, 0)

	t.Run("known available skips reported full", func(t *testing.T) {
		svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
		svc.nowFn = func() time.Time { return now }
		full := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(1, 1))
		available := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 1))

		picked, err := svc.pickSessionForCapability(taskCapabilityTerminalExec, "owner-a", sessionPickOptions{
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		if err != nil {
			t.Fatalf("pick terminal worker: %v", err)
		}
		defer picked.releaseCapability(taskCapabilityTerminalExec)
		if picked != available {
			t.Fatalf("expected available worker %s, got %s (full=%s)", available.nodeID, picked.nodeID, full.nodeID)
		}
	})

	t.Run("known available precedes legacy unknown despite inflight", func(t *testing.T) {
		svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
		svc.nowFn = func() time.Time { return now }
		unknown := addTerminalCapacityTestWorker(t, svc, "node-a", now, nil)
		available := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
		if !available.tryAcquireCapability(taskCapabilityTerminalExec) {
			t.Fatal("pre-acquire available worker capability")
		}
		defer available.releaseCapability(taskCapabilityTerminalExec)

		picked, err := svc.pickSessionForCapability(taskCapabilityTerminalExec, "owner-a", sessionPickOptions{
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		if err != nil {
			t.Fatalf("pick terminal worker: %v", err)
		}
		defer picked.releaseCapability(taskCapabilityTerminalExec)
		if picked != available {
			t.Fatalf("expected known available worker %s before unknown %s, got %s", available.nodeID, unknown.nodeID, picked.nodeID)
		}
	})

	t.Run("unknown intent preserves inflight selection", func(t *testing.T) {
		svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
		svc.nowFn = func() time.Time { return now }
		full := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(1, 1))
		available := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
		if !available.tryAcquireCapability(taskCapabilityTerminalExec) {
			t.Fatal("pre-acquire available worker capability")
		}
		defer available.releaseCapability(taskCapabilityTerminalExec)

		picked, err := svc.pickSessionForCapability(taskCapabilityTerminalExec, "owner-a", sessionPickOptions{})
		if err != nil {
			t.Fatalf("pick terminal worker: %v", err)
		}
		defer picked.releaseCapability(taskCapabilityTerminalExec)
		if picked != full {
			t.Fatalf("expected lower-inflight full worker %s for unknown intent, got %s", full.nodeID, picked.nodeID)
		}
	})

	t.Run("reported full remains last resort", func(t *testing.T) {
		svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
		svc.nowFn = func() time.Time { return now }
		addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(1, 1))
		addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(3, 3))

		picked, err := svc.pickSessionForCapability(taskCapabilityTerminalExec, "owner-a", sessionPickOptions{
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		if err != nil {
			t.Fatalf("expected last-resort worker, got %v", err)
		}
		picked.releaseCapability(taskCapabilityTerminalExec)
	})

	t.Run("explicit unlimited is known available", func(t *testing.T) {
		svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
		svc.nowFn = func() time.Time { return now }
		unlimited := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(0, 99))
		addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(1, 1))

		picked, err := svc.pickSessionForCapability(taskCapabilityTerminalExec, "owner-a", sessionPickOptions{
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		if err != nil {
			t.Fatalf("pick terminal worker: %v", err)
		}
		defer picked.releaseCapability(taskCapabilityTerminalExec)
		if picked != unlimited {
			t.Fatalf("expected explicit unlimited worker %s, got %s", unlimited.nodeID, picked.nodeID)
		}
	})
}

func TestExistingTerminalRouteIgnoresActiveSessionCapacity(t *testing.T) {
	now := time.Unix(1_700_100_100, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	full := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(1, 1))
	addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
	svc.bindTerminalSessionRoute("session-existing", full.nodeID, now)

	picked, reservationID, err := svc.pickSessionForDispatch(
		taskCapabilityTerminalExec,
		"owner-a",
		"session-existing",
		sessionPickOptions{terminalSessionIntent: terminalSessionIntentKnownNew},
	)
	if err != nil {
		t.Fatalf("pick existing route: %v", err)
	}
	defer picked.releaseCapability(taskCapabilityTerminalExec)
	if picked != full || reservationID != 0 {
		t.Fatalf("expected confirmed route on %s, got node=%s reservation=%d", full.nodeID, picked.nodeID, reservationID)
	}
}

func TestTerminalCapacityDoesNotAffectOtherCapabilities(t *testing.T) {
	now := time.Unix(1_700_100_200, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	full := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(1, 1))
	available := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
	if !available.tryAcquireCapability(echoCapabilityName) {
		t.Fatal("pre-acquire echo capability")
	}
	defer available.releaseCapability(echoCapabilityName)

	picked, err := svc.pickSessionForCapability(echoCapabilityName, "", sessionPickOptions{
		terminalSessionIntent: terminalSessionIntentKnownNew,
	})
	if err != nil {
		t.Fatalf("pick echo worker: %v", err)
	}
	defer picked.releaseCapability(echoCapabilityName)
	if picked != full {
		t.Fatalf("expected normal inflight selection on %s, got %s", full.nodeID, picked.nodeID)
	}
}

func TestDispatchCommandRetriesTerminalCapacityOnAnotherWorker(t *testing.T) {
	now := time.Unix(1_700_100_300, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(2, 0))
	workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))

	commandIDs := []string{"command-a", "command-b"}
	svc.newCommandIDFn = func() (string, error) {
		if len(commandIDs) == 0 {
			return "", errors.New("no command IDs")
		}
		id := commandIDs[0]
		commandIDs = commandIDs[1:]
		return id, nil
	}

	payload := []byte(`{"command":"pwd","session_id":"session-retry","create_if_missing":true}`)
	type result struct {
		outcome commandOutcome
		err     error
	}
	done := make(chan result, 1)
	var dispatchedMu sync.Mutex
	dispatched := make([]string, 0, 2)
	go func() {
		outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
			ownerID:               "owner-a",
			taskID:                "task-retry",
			terminalSessionIntent: terminalSessionIntentKnownNew,
			onDispatched: func(commandID string) error {
				dispatchedMu.Lock()
				defer dispatchedMu.Unlock()
				dispatched = append(dispatched, commandID)
				return nil
			},
		})
		done <- result{outcome: outcome, err: err}
	}()

	dispatchA := receiveCommandDispatch(t, workerA)
	workerA.resolvePending(&registryv1.CommandResult{
		CommandId: dispatchA.GetCommandId(),
		Error: &registryv1.CommandError{
			Code:    terminalSessionCapacityExceededCode,
			Message: "terminal session capacity exceeded",
		},
		CompletedUnixMs: now.UnixMilli(),
	})
	dispatchB := receiveCommandDispatch(t, workerB)
	if dispatchA.GetCommandId() == dispatchB.GetCommandId() {
		t.Fatal("expected a fresh command ID for retry")
	}
	if dispatchA.GetDeadlineUnixMs() == 0 || dispatchA.GetDeadlineUnixMs() != dispatchB.GetDeadlineUnixMs() {
		t.Fatalf("expected attempts to share one deadline, got %d and %d", dispatchA.GetDeadlineUnixMs(), dispatchB.GetDeadlineUnixMs())
	}
	workerB.resolvePending(&registryv1.CommandResult{
		CommandId:       dispatchB.GetCommandId(),
		PayloadJson:     []byte(`{"session_id":"session-retry","stdout":"ok"}`),
		CompletedUnixMs: now.UnixMilli(),
	})

	resultValue := <-done
	if resultValue.err != nil || resultValue.outcome.err != nil {
		t.Fatalf("retry did not succeed: outcome=%v err=%v", resultValue.outcome.err, resultValue.err)
	}
	dispatchedMu.Lock()
	gotDispatched := append([]string(nil), dispatched...)
	dispatchedMu.Unlock()
	if len(gotDispatched) != 2 || gotDispatched[0] != "command-a" || gotDispatched[1] != "command-b" {
		t.Fatalf("unexpected dispatched command IDs: %#v", gotDispatched)
	}

	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["session-retry"]
	svc.terminalRoutesMu.RUnlock()
	if route.NodeID != workerB.nodeID || route.ReservationID != 0 || route.ProvisionalUses != 0 {
		t.Fatalf("retry did not confirm route on worker B: %#v", route)
	}
}

func TestSubmitTaskSkipsReportedFullConnectedWorker(t *testing.T) {
	now := time.Unix(1_700_100_325, 0)
	svc := NewRegistryService(
		registrytest.NewStore(t),
		map[string]string{"node-a": "secret-a", "node-b": "secret-b"},
		5,
		15,
		time.Minute,
	)
	svc.nowFn = func() time.Time { return now }
	svc.newTaskIDFn = func() (string, error) { return "task-connected-skip", nil }
	svc.newTerminalSessionIDFn = func() (string, error) { return "session-connected-skip", nil }

	client, cleanup := newBufClient(t, svc)
	defer cleanup()
	streamA, _, err := connectWorkerWithHello(client, &registryv1.ConnectHello{
		NodeId:                  "node-a",
		WorkerSecret:            "secret-a",
		Capabilities:            []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 4}},
		TerminalSessionCapacity: terminalCapacity(1, 1),
	})
	if err != nil {
		t.Fatalf("connect full worker: %v", err)
	}
	defer streamA.CloseSend()
	streamB, _, err := connectWorkerWithHello(client, &registryv1.ConnectHello{
		NodeId:                  "node-b",
		WorkerSecret:            "secret-b",
		Capabilities:            []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 4}},
		TerminalSessionCapacity: terminalCapacity(2, 0),
	})
	if err != nil {
		t.Fatalf("connect available worker: %v", err)
	}
	defer streamB.CloseSend()

	submitted, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: taskCapabilityTerminalExec,
		InputJSON:  []byte(`{"command":"pwd"}`),
		Mode:       TaskModeAsync,
		Timeout:    2 * time.Second,
		OwnerID:    "owner-a",
	})
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}
	if submitted.Task.TaskID != "task-connected-skip" {
		t.Fatalf("unexpected task ID: %s", submitted.Task.TaskID)
	}

	workerAResponse := make(chan *registryv1.ConnectResponse, 1)
	workerAErr := make(chan error, 1)
	go func() {
		response, recvErr := streamA.Recv()
		if recvErr != nil {
			workerAErr <- recvErr
			return
		}
		workerAResponse <- response
	}()

	responseB, err := streamB.Recv()
	if err != nil || responseB.GetCommandDispatch() == nil {
		t.Fatalf("receive available worker dispatch: response=%#v err=%v", responseB, err)
	}
	dispatchB := responseB.GetCommandDispatch()
	select {
	case response := <-workerAResponse:
		t.Fatalf("reported-full worker received dispatch: %#v", response.GetPayload())
	case <-workerAErr:
		t.Fatal("reported-full worker stream closed unexpectedly")
	case <-time.After(50 * time.Millisecond):
	}

	if err := streamB.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       dispatchB.GetCommandId(),
				PayloadJson:     []byte(`{"session_id":"obx:owner-a:session-connected-skip","stdout":"ok"}`),
				CompletedUnixMs: now.UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send available worker result: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, found := svc.GetTask("task-connected-skip", "owner-a")
		if !found {
			t.Fatal("task disappeared")
		}
		if snapshot.Status == TaskStatusSucceeded {
			break
		}
		if isTaskTerminal(snapshot.Status) {
			t.Fatalf("capacity-aware task failed: %#v", snapshot)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for capacity-aware task: %#v", snapshot)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSubmitTaskRetriesCapacityAcrossConnectedWorkers(t *testing.T) {
	now := time.Unix(1_700_100_350, 0)
	svc := NewRegistryService(
		registrytest.NewStore(t),
		map[string]string{"node-a": "secret-a", "node-b": "secret-b"},
		5,
		15,
		time.Minute,
	)
	svc.nowFn = func() time.Time { return now }
	svc.newTaskIDFn = func() (string, error) { return "task-connected-retry", nil }
	svc.newTerminalSessionIDFn = func() (string, error) { return "external-session", nil }
	commandIDs := []string{"connected-command-a", "connected-command-b"}
	svc.newCommandIDFn = func() (string, error) {
		if len(commandIDs) == 0 {
			return "", errors.New("no command IDs")
		}
		id := commandIDs[0]
		commandIDs = commandIDs[1:]
		return id, nil
	}

	client, cleanup := newBufClient(t, svc)
	defer cleanup()
	connect := func(nodeID string, secret string) (workerStream interface {
		Send(*registryv1.ConnectRequest) error
		Recv() (*registryv1.ConnectResponse, error)
		CloseSend() error
	}, sessionID string) {
		t.Helper()
		stream, connectedSessionID, err := connectWorkerWithHello(client, &registryv1.ConnectHello{
			NodeId:       nodeID,
			WorkerSecret: secret,
			Capabilities: []*registryv1.CapabilityDeclaration{{
				Name:        taskCapabilityTerminalExec,
				MaxInflight: 4,
			}},
			TerminalSessionCapacity: terminalCapacity(2, 0),
		})
		if err != nil {
			t.Fatalf("connect %s: %v", nodeID, err)
		}
		return stream, connectedSessionID
	}
	streamA, sessionA := connect("node-a", "secret-a")
	defer streamA.CloseSend()
	streamB, sessionB := connect("node-b", "secret-b")
	defer streamB.CloseSend()

	for _, heartbeat := range []struct {
		stream interface {
			Send(*registryv1.ConnectRequest) error
			Recv() (*registryv1.ConnectResponse, error)
		}
		nodeID    string
		sessionID string
	}{
		{stream: streamA, nodeID: "node-a", sessionID: sessionA},
		{stream: streamB, nodeID: "node-b", sessionID: sessionB},
	} {
		if err := heartbeat.stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_Heartbeat{
				Heartbeat: &registryv1.HeartbeatFrame{
					NodeId:             heartbeat.nodeID,
					SessionId:          heartbeat.sessionID,
					ActiveSessionCount: 0,
				},
			},
		}); err != nil {
			t.Fatalf("send heartbeat for %s: %v", heartbeat.nodeID, err)
		}
		response, err := heartbeat.stream.Recv()
		if err != nil || response.GetHeartbeatAck() == nil {
			t.Fatalf("heartbeat ack for %s: response=%#v err=%v", heartbeat.nodeID, response, err)
		}
	}

	submitted, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: taskCapabilityTerminalExec,
		InputJSON:  []byte(`{"command":"pwd"}`),
		Mode:       TaskModeAsync,
		Timeout:    2 * time.Second,
		OwnerID:    "owner-a",
	})
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}
	if submitted.Task.TaskID != "task-connected-retry" {
		t.Fatalf("unexpected task ID: %s", submitted.Task.TaskID)
	}

	responseA, err := streamA.Recv()
	if err != nil || responseA.GetCommandDispatch() == nil {
		t.Fatalf("receive worker A dispatch: response=%#v err=%v", responseA, err)
	}
	dispatchA := responseA.GetCommandDispatch()
	if dispatchA.GetCommandId() != "connected-command-a" {
		t.Fatalf("unexpected worker A command ID: %s", dispatchA.GetCommandId())
	}
	if err := streamA.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId: dispatchA.GetCommandId(),
				Error: &registryv1.CommandError{
					Code:    terminalSessionCapacityExceededCode,
					Message: "terminal session capacity exceeded",
				},
				CompletedUnixMs: now.UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send worker A capacity result: %v", err)
	}

	responseB, err := streamB.Recv()
	if err != nil || responseB.GetCommandDispatch() == nil {
		t.Fatalf("receive worker B dispatch: response=%#v err=%v", responseB, err)
	}
	dispatchB := responseB.GetCommandDispatch()
	if dispatchB.GetCommandId() != "connected-command-b" {
		t.Fatalf("unexpected worker B command ID: %s", dispatchB.GetCommandId())
	}
	var scopedPayload terminalExecScopedPayload
	if err := json.Unmarshal(dispatchB.GetPayloadJson(), &scopedPayload); err != nil {
		t.Fatalf("decode scoped terminal payload: %v", err)
	}
	if scopedPayload.SessionID != "obx:owner-a:external-session" || !scopedPayload.CreateIfMissing {
		t.Fatalf("unexpected scoped terminal payload: %#v", scopedPayload)
	}
	if err := streamB.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       dispatchB.GetCommandId(),
				PayloadJson:     []byte(`{"session_id":"obx:owner-a:external-session","stdout":"ok"}`),
				CompletedUnixMs: now.UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send worker B success result: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, found := svc.GetTask("task-connected-retry", "owner-a")
		if !found {
			t.Fatal("task disappeared")
		}
		if snapshot.Status == TaskStatusSucceeded {
			if snapshot.CommandID != "connected-command-b" {
				t.Fatalf("task retained stale command ID: %s", snapshot.CommandID)
			}
			var resultPayload map[string]any
			if err := json.Unmarshal(snapshot.ResultJSON, &resultPayload); err != nil {
				t.Fatalf("decode task result: %v", err)
			}
			if resultPayload["session_id"] != "external-session" || resultPayload["stdout"] != "ok" {
				t.Fatalf("unexpected task result: %#v", resultPayload)
			}
			break
		}
		if isTaskTerminal(snapshot.Status) {
			t.Fatalf("task failed after retry: %#v", snapshot)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for task success: %#v", snapshot)
		}
		time.Sleep(10 * time.Millisecond)
	}

	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["obx:owner-a:external-session"]
	svc.terminalRoutesMu.RUnlock()
	if route.NodeID != "node-b" || route.ReservationID != 0 {
		t.Fatalf("task retry route was not confirmed on worker B: %#v", route)
	}
}

func TestTerminalCapacityRetryWorksForAllTaskModes(t *testing.T) {
	modes := []TaskMode{TaskModeSync, TaskModeAuto, TaskModeAsync}
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			now := time.Unix(1_700_100_360, 0)
			svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
			svc.nowFn = func() time.Time { return now }
			svc.newTaskIDFn = func() (string, error) { return "task-mode-" + string(mode), nil }
			svc.newTerminalSessionIDFn = func() (string, error) { return "session-mode-" + string(mode), nil }
			workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(2, 0))
			workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))

			type submitResult struct {
				result SubmitTaskResult
				err    error
			}
			submitDone := make(chan submitResult, 1)
			go func() {
				result, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
					Capability: taskCapabilityTerminalExec,
					InputJSON:  []byte(`{"command":"pwd"}`),
					Mode:       mode,
					Wait:       time.Second,
					Timeout:    2 * time.Second,
					RequestID:  "request-mode-" + string(mode),
					OwnerID:    "owner-a",
				})
				submitDone <- submitResult{result: result, err: err}
			}()

			dispatchA := receiveCommandDispatch(t, workerA)
			workerA.resolvePending(&registryv1.CommandResult{
				CommandId: dispatchA.GetCommandId(),
				Error: &registryv1.CommandError{
					Code:    terminalSessionCapacityExceededCode,
					Message: "terminal session capacity exceeded",
				},
				CompletedUnixMs: now.UnixMilli(),
			})
			dispatchB := receiveCommandDispatch(t, workerB)
			workerB.resolvePending(&registryv1.CommandResult{
				CommandId: dispatchB.GetCommandId(),
				PayloadJson: []byte(`{"session_id":"obx:owner-a:session-mode-` +
					string(mode) + `","stdout":"ok"}`),
				CompletedUnixMs: now.UnixMilli(),
			})

			submitted := <-submitDone
			if submitted.err != nil {
				t.Fatalf("submit %s task: %v", mode, submitted.err)
			}
			if mode != TaskModeAsync && (!submitted.result.Completed || submitted.result.Task.Status != TaskStatusSucceeded) {
				t.Fatalf("%s submit did not return completed success: %#v", mode, submitted.result)
			}

			deadline := time.Now().Add(2 * time.Second)
			for {
				snapshot, found := svc.GetTask("task-mode-"+string(mode), "owner-a")
				if !found {
					t.Fatal("task disappeared")
				}
				if snapshot.Status == TaskStatusSucceeded {
					if snapshot.RequestID != "request-mode-"+string(mode) || snapshot.CommandID != dispatchB.GetCommandId() {
						t.Fatalf("task identity changed across retry: %#v", snapshot)
					}
					break
				}
				if isTaskTerminal(snapshot.Status) {
					t.Fatalf("%s task ended unsuccessfully: %#v", mode, snapshot)
				}
				if time.Now().After(deadline) {
					t.Fatalf("timed out waiting for %s task: %#v", mode, snapshot)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestDispatchCommandDoesNotRetryNonCapacityOrAmbiguousFailures(t *testing.T) {
	now := time.Unix(1_700_100_375, 0)

	t.Run("session busy", func(t *testing.T) {
		svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
		svc.nowFn = func() time.Time { return now }
		workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(2, 0))
		workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
		payload := []byte(`{"command":"pwd","session_id":"session-busy","create_if_missing":true}`)
		type result struct {
			outcome commandOutcome
			err     error
		}
		done := make(chan result, 1)
		go func() {
			outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
				ownerID:               "owner-a",
				terminalSessionIntent: terminalSessionIntentKnownNew,
			})
			done <- result{outcome: outcome, err: err}
		}()

		dispatch := receiveCommandDispatch(t, workerA)
		workerA.resolvePending(&registryv1.CommandResult{
			CommandId: dispatch.GetCommandId(),
			Error: &registryv1.CommandError{
				Code:    "session_busy",
				Message: "session busy",
			},
			CompletedUnixMs: now.UnixMilli(),
		})
		resultValue := <-done
		if resultValue.err != nil || !isCommandErrorCode(resultValue.outcome.err, "session_busy") {
			t.Fatalf("unexpected session_busy result: outcome=%v err=%v", resultValue.outcome.err, resultValue.err)
		}
		assertNoCommandDispatch(t, workerB)
	})

	t.Run("worker stream closes after enqueue", func(t *testing.T) {
		svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
		svc.nowFn = func() time.Time { return now }
		workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(2, 0))
		workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
		payload := []byte(`{"command":"pwd","session_id":"session-stream","create_if_missing":true}`)
		type result struct {
			outcome commandOutcome
			err     error
		}
		done := make(chan result, 1)
		go func() {
			outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
				ownerID:               "owner-a",
				terminalSessionIntent: terminalSessionIntentKnownNew,
			})
			done <- result{outcome: outcome, err: err}
		}()

		_ = receiveCommandDispatch(t, workerA)
		workerA.close(errors.New("worker stream closed"))
		resultValue := <-done
		if resultValue.err != nil || resultValue.outcome.err == nil || isSessionCapacityCommandError(resultValue.outcome.err) {
			t.Fatalf("unexpected stream-close result: outcome=%v err=%v", resultValue.outcome.err, resultValue.err)
		}
		assertNoCommandDispatch(t, workerB)
	})
}

func TestConcurrentProvisionalCapacityOnlyLastRollbackRetries(t *testing.T) {
	now := time.Unix(1_700_100_390, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(2, 0))
	workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
	payload := []byte(`{"command":"pwd","session_id":"session-shared-retry","create_if_missing":true}`)
	type result struct {
		outcome commandOutcome
		err     error
	}
	firstDone := make(chan result, 1)
	secondDone := make(chan result, 1)

	go func() {
		outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
			ownerID:               "owner-a",
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		firstDone <- result{outcome: outcome, err: err}
	}()
	firstDispatch := receiveCommandDispatch(t, workerA)

	go func() {
		outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
			ownerID:               "owner-a",
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		secondDone <- result{outcome: outcome, err: err}
	}()
	secondDispatch := receiveCommandDispatch(t, workerA)

	workerA.resolvePending(&registryv1.CommandResult{
		CommandId: firstDispatch.GetCommandId(),
		Error: &registryv1.CommandError{
			Code:    terminalSessionCapacityExceededCode,
			Message: "terminal session capacity exceeded",
		},
		CompletedUnixMs: now.UnixMilli(),
	})
	first := <-firstDone
	if first.err != nil || !isSessionCapacityCommandError(first.outcome.err) {
		t.Fatalf("first shared attempt unexpectedly retried: outcome=%v err=%v", first.outcome.err, first.err)
	}
	assertNoCommandDispatch(t, workerB)

	workerA.resolvePending(&registryv1.CommandResult{
		CommandId: secondDispatch.GetCommandId(),
		Error: &registryv1.CommandError{
			Code:    terminalSessionCapacityExceededCode,
			Message: "terminal session capacity exceeded",
		},
		CompletedUnixMs: now.UnixMilli(),
	})
	retryDispatch := receiveCommandDispatch(t, workerB)
	workerB.resolvePending(&registryv1.CommandResult{
		CommandId:       retryDispatch.GetCommandId(),
		PayloadJson:     []byte(`{"session_id":"session-shared-retry","stdout":"ok"}`),
		CompletedUnixMs: now.UnixMilli(),
	})
	second := <-secondDone
	if second.err != nil || second.outcome.err != nil {
		t.Fatalf("last provisional user did not retry successfully: outcome=%v err=%v", second.outcome.err, second.err)
	}

	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["session-shared-retry"]
	svc.terminalRoutesMu.RUnlock()
	if route.NodeID != workerB.nodeID || route.ReservationID != 0 || route.ProvisionalUses != 0 {
		t.Fatalf("shared retry did not confirm worker B route: %#v", route)
	}
}

func TestDispatchCommandPreservesCapacityErrorAfterAllWorkersReject(t *testing.T) {
	now := time.Unix(1_700_100_400, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(1, 1))
	workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(1, 1))

	payload := []byte(`{"command":"pwd","session_id":"session-full","create_if_missing":true}`)
	type result struct {
		outcome commandOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
			ownerID:               "owner-a",
			taskID:                "task-full",
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		done <- result{outcome: outcome, err: err}
	}()

	for _, worker := range []*activeSession{workerA, workerB} {
		dispatch := receiveCommandDispatch(t, worker)
		worker.resolvePending(&registryv1.CommandResult{
			CommandId: dispatch.GetCommandId(),
			Error: &registryv1.CommandError{
				Code:    terminalSessionCapacityExceededCode,
				Message: "terminal session capacity exceeded",
			},
			CompletedUnixMs: now.UnixMilli(),
		})
	}

	resultValue := <-done
	if resultValue.err != nil || !isSessionCapacityCommandError(resultValue.outcome.err) {
		t.Fatalf("expected final session capacity error, outcome=%v err=%v", resultValue.outcome.err, resultValue.err)
	}
	if _, ok := svc.touchTerminalSessionRoute("session-full", now.Add(time.Second)); ok {
		t.Fatal("capacity retry exhaustion left a terminal route")
	}
}

func TestDispatchCommandPreservesSessionCapacityWhenRemainingWorkerInflightIsFull(t *testing.T) {
	now := time.Unix(1_700_100_450, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(2, 0))
	workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
	for i := 0; i < 4; i++ {
		if !workerB.tryAcquireCapability(taskCapabilityTerminalExec) {
			t.Fatalf("pre-acquire worker B capability %d", i)
		}
		defer workerB.releaseCapability(taskCapabilityTerminalExec)
	}

	payload := []byte(`{"command":"pwd","session_id":"session-mixed-capacity","create_if_missing":true}`)
	type result struct {
		outcome commandOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
			ownerID:               "owner-a",
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		done <- result{outcome: outcome, err: err}
	}()

	dispatch := receiveCommandDispatch(t, workerA)
	workerA.resolvePending(&registryv1.CommandResult{
		CommandId: dispatch.GetCommandId(),
		Error: &registryv1.CommandError{
			Code:    terminalSessionCapacityExceededCode,
			Message: "terminal session capacity exceeded",
		},
		CompletedUnixMs: now.UnixMilli(),
	})
	resultValue := <-done
	if resultValue.err != nil || !isSessionCapacityCommandError(resultValue.outcome.err) {
		t.Fatalf("expected concrete session capacity error, outcome=%v err=%v", resultValue.outcome.err, resultValue.err)
	}
	assertNoCommandDispatch(t, workerB)
}

func TestDispatchCommandCancellationAfterEnqueueDoesNotTryAnotherWorker(t *testing.T) {
	now := time.Unix(1_700_100_475, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(2, 0))
	workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	payload := []byte(`{"command":"pwd","session_id":"session-cancel","create_if_missing":true}`)
	type result struct {
		outcome commandOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := svc.dispatchCommand(ctx, taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
			ownerID:               "owner-a",
			terminalSessionIntent: terminalSessionIntentKnownNew,
			onDispatched: func(string) error {
				cancel()
				return nil
			},
		})
		done <- result{outcome: outcome, err: err}
	}()

	_ = receiveCommandDispatch(t, workerA)
	resultValue := <-done
	if !errors.Is(resultValue.err, context.Canceled) {
		t.Fatalf("expected context cancellation, outcome=%v err=%v", resultValue.outcome.err, resultValue.err)
	}
	assertNoCommandDispatch(t, workerB)
}

func TestDispatchCommandDoesNotRetryCapacityForConfirmedRoute(t *testing.T) {
	now := time.Unix(1_700_100_500, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(1, 1))
	workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
	svc.bindTerminalSessionRoute("session-pinned", workerA.nodeID, now)

	payload := []byte(`{"command":"pwd","session_id":"session-pinned","create_if_missing":true}`)
	type result struct {
		outcome commandOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
			ownerID:               "owner-a",
			terminalSessionIntent: terminalSessionIntentKnownNew,
		})
		done <- result{outcome: outcome, err: err}
	}()

	dispatch := receiveCommandDispatch(t, workerA)
	workerA.resolvePending(&registryv1.CommandResult{
		CommandId: dispatch.GetCommandId(),
		Error: &registryv1.CommandError{
			Code:    terminalSessionCapacityExceededCode,
			Message: "terminal session capacity exceeded",
		},
		CompletedUnixMs: now.UnixMilli(),
	})
	resultValue := <-done
	if resultValue.err != nil || !isSessionCapacityCommandError(resultValue.outcome.err) {
		t.Fatalf("expected pinned capacity error, outcome=%v err=%v", resultValue.outcome.err, resultValue.err)
	}
	assertNoCommandDispatch(t, workerB)
	if nodeID, ok := svc.touchTerminalSessionRoute("session-pinned", now.Add(time.Second)); !ok || nodeID != workerA.nodeID {
		t.Fatalf("confirmed route changed after capacity error: node=%q ok=%t", nodeID, ok)
	}
}

func TestDispatchCommandStopsRetryWhenTaskPersistenceCallbackFails(t *testing.T) {
	now := time.Unix(1_700_100_600, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	workerA := addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(2, 0))
	workerB := addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
	persistErr := errors.New("persist running state")

	payload := []byte(`{"command":"pwd","session_id":"session-persist","create_if_missing":true}`)
	_, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, 2*time.Second, dispatchOptions{
		ownerID:               "owner-a",
		terminalSessionIntent: terminalSessionIntentKnownNew,
		onDispatched: func(string) error {
			return persistErr
		},
	})
	if !errors.Is(err, persistErr) {
		t.Fatalf("expected persistence callback error, got %v", err)
	}
	_ = receiveCommandDispatch(t, workerA)
	assertNoCommandDispatch(t, workerB)
	if nodeID, ok := svc.touchTerminalSessionRoute("session-persist", now.Add(time.Second)); !ok || nodeID != workerA.nodeID {
		t.Fatalf("dispatched command route was not conservatively confirmed: node=%q ok=%t", nodeID, ok)
	}
}

func TestPickSessionForDispatchStopsWhenRouteReturnsToAttemptedNode(t *testing.T) {
	now := time.Unix(1_700_100_700, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	addTerminalCapacityTestWorker(t, svc, "node-a", now, terminalCapacity(1, 1))
	addTerminalCapacityTestWorker(t, svc, "node-b", now, terminalCapacity(2, 0))
	_, reservationID := svc.reserveTerminalSessionRoute("session-race", "node-a", now)

	_, _, err := svc.pickSessionForDispatch(taskCapabilityTerminalExec, "owner-a", "session-race", sessionPickOptions{
		terminalSessionIntent: terminalSessionIntentKnownNew,
		excludedNodeIDs:       map[string]struct{}{"node-a": {}},
	})
	if !errors.Is(err, errTerminalSessionRetryRouteConflict) {
		t.Fatalf("expected retry route conflict, got %v", err)
	}

	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["session-race"]
	svc.terminalRoutesMu.RUnlock()
	if route.NodeID != "node-a" || route.ReservationID != reservationID || route.ProvisionalUses != 1 {
		t.Fatalf("retry route conflict changed the newer route: %#v", route)
	}
}
