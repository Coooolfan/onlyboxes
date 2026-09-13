package grpcserver

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type preparedTaskInput struct {
	inputJSON          []byte
	dispatchCapability string
	targetNodeID       string
	immediateResult    []byte
}

type workerSysListItem struct {
	WorkerID     string                           `json:"worker_id"`
	NodeName     string                           `json:"node_name"`
	Status       registry.WorkerStatus            `json:"status"`
	Capabilities []registry.CapabilityDeclaration `json:"capabilities"`
}

type workerSysListResult struct {
	WorkerList []workerSysListItem `json:"worker_list"`
}

func (s *RegistryService) prepareTaskInput(capability string, ownerID string, inputJSON []byte) (preparedTaskInput, error) {
	prepared := preparedTaskInput{inputJSON: append([]byte(nil), inputJSON...), dispatchCapability: normalizeCapability(capability)}
	prefix := s.computerUseSessionIDPrefix
	if strings.TrimSpace(prefix) == "" {
		prefix = defaultComputerUseSessionIDPrefix
	}

	switch normalizeCapability(capability) {
	case computerUseCapabilityName:
		payload := map[string]json.RawMessage{}
		if err := json.Unmarshal(inputJSON, &payload); err != nil || payload == nil {
			return prepared, status.Error(codes.InvalidArgument, "invalid computerUse payload")
		}
		workerIDRaw, hasWorkerID := payload["worker_id"]
		if !hasWorkerID {
			result, err := s.workerSysListResult(ownerID)
			if err != nil {
				return prepared, err
			}
			prepared.immediateResult = result
			return prepared, nil
		}
		workerID := rawJSONString(workerIDRaw)
		if workerID == "" {
			return prepared, status.Error(codes.InvalidArgument, "worker_id is invalid")
		}
		command := rawJSONString(payload["command"])
		if command == "" {
			return prepared, status.Error(codes.InvalidArgument, "command is required when worker_id is provided")
		}
		if err := s.validateWorkerSysTarget(ownerID, workerID); err != nil {
			return prepared, err
		}
		delete(payload, "worker_id")
		normalized, err := json.Marshal(payload)
		if err != nil {
			return prepared, status.Error(codes.Internal, "failed to encode computerUse payload")
		}
		prepared.inputJSON = normalized
		prepared.targetNodeID = workerID
		return prepared, nil

	case readImageCapabilityName:
		payload := map[string]json.RawMessage{}
		if err := json.Unmarshal(inputJSON, &payload); err != nil {
			return prepared, status.Error(codes.InvalidArgument, "invalid readImage payload")
		}
		sessionID := rawJSONString(payload["session_id"])
		if !strings.HasPrefix(sessionID, prefix) {
			prepared.dispatchCapability = taskCapabilityTerminalResource
			return prepared, nil
		}
		workerID := strings.TrimSpace(strings.TrimPrefix(sessionID, prefix))
		if workerID == "" {
			return prepared, status.Errorf(codes.InvalidArgument, "session_id must include a worker_id after reserved prefix %q", prefix)
		}
		if err := s.validateWorkerSysTarget(ownerID, workerID); err != nil {
			return prepared, err
		}
		payload["session_id"], _ = json.Marshal("computerUse")
		normalized, err := json.Marshal(payload)
		if err != nil {
			return prepared, status.Error(codes.Internal, "failed to encode readImage payload")
		}
		prepared.inputJSON = normalized
		prepared.targetNodeID = workerID
		return prepared, nil

	case taskCapabilityTerminalExec:
		payload := terminalExecScopedPayload{}
		if err := json.Unmarshal(inputJSON, &payload); err != nil {
			return prepared, nil
		}
		sessionID := strings.TrimSpace(payload.SessionID)
		if payload.CreateIfMissing && sessionID != "" && strings.HasPrefix(sessionID, prefix) {
			return prepared, status.Errorf(codes.InvalidArgument, "invalid_payload: session_id uses reserved identifier prefix %q", prefix)
		}
	}
	return prepared, nil
}

func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func (s *RegistryService) validateWorkerSysTarget(ownerID string, workerID string) error {
	if s == nil || s.store == nil {
		return status.Error(codes.Unavailable, "worker registry is unavailable")
	}
	worker, found := s.store.GetByNodeID(workerID, s.nowFn(), time.Duration(s.offlineTTLSec)*time.Second)
	if !found || strings.TrimSpace(worker.Labels[registry.LabelOwnerIDKey]) != strings.TrimSpace(ownerID) ||
		strings.ToLower(strings.TrimSpace(worker.Labels[registry.LabelWorkerTypeKey])) != registry.WorkerTypeSys {
		return status.Error(codes.InvalidArgument, "worker_id is invalid")
	}
	return nil
}

func (s *RegistryService) workerSysListResult(ownerID string) ([]byte, error) {
	workers := s.store.ListByOwnerAndType(ownerID, registry.WorkerTypeSys, s.nowFn(), time.Duration(s.offlineTTLSec)*time.Second)
	items := make([]workerSysListItem, 0, len(workers))
	for _, worker := range workers {
		items = append(items, workerSysListItem{
			WorkerID: worker.NodeID, NodeName: worker.NodeName, Status: worker.Status,
			Capabilities: append([]registry.CapabilityDeclaration(nil), worker.Capabilities...),
		})
	}
	result, err := json.Marshal(workerSysListResult{WorkerList: items})
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to encode worker list")
	}
	return result, nil
}

func (s *RegistryService) checkTargetCapabilityAvailability(nodeID string, capability string) error {
	session := s.getSession(strings.TrimSpace(nodeID))
	if session == nil || !session.isReady() {
		return ErrTargetWorkerOffline
	}
	if !session.hasCapability(capability) {
		return ErrTargetWorkerCapabilityUnavailable
	}
	inflight, maxInflight, ok := session.inflightSnapshot(capability)
	if !ok || inflight >= maxInflight {
		return ErrNoWorkerCapacity
	}
	return nil
}
