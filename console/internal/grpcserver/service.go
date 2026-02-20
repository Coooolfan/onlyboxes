package grpcserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

const (
	maxNodeIDLength               = 128
	echoCapabilityName            = "echo"
	defaultEchoTimeout            = 5 * time.Second
	defaultCloseMessage           = "session closed"
	defaultCapabilityMaxInflight  = 4
	maxProvisioningCreateAttempts = 8
	heartbeatAckEnqueueTimeout    = 500 * time.Millisecond
	controlOutboundBufferSize     = 32
	commandOutboundBufferSize     = 128
	defaultTaskRetentionWindow    = 10 * time.Minute
	defaultCommandDispatchTimeout = 60 * time.Second
)

var ErrNoEchoWorker = errors.New("no online worker supports echo")
var ErrEchoTimeout = errors.New("echo command timed out")
var ErrNoCapabilityWorker = errors.New("no online worker supports capability")
var ErrNoWorkerCapacity = errors.New("no online worker capacity for capability")
var ErrTaskRequestInProgress = errors.New("task request already in progress")

type RegistryService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer

	store                  *registry.Store
	credentialsMu          sync.RWMutex
	credentials            map[string]string
	credentialHashAlgo     string
	hasher                 *persistence.Hasher
	heartbeatIntervalSec   int32
	offlineTTLSec          int32
	nowFn                  func() time.Time
	newSessionIDFn         func() (string, error)
	newCommandIDFn         func() (string, error)
	newTaskIDFn            func() (string, error)
	newTerminalSessionIDFn func() (string, error)
	taskRetention          time.Duration

	sessionsMu sync.RWMutex
	sessions   map[string]*activeSession
	roundRobin uint64

	tasksMu sync.RWMutex
	// Active task runtime index:
	// - tasks stores in-flight task runtime records by task_id.
	// - taskRequestReservations tracks (owner_id, request_id) currently being validated/created.
	tasks                        map[string]*taskRecord
	taskRequestReservations      map[string]struct{}
	criticalPersistenceFailureFn func(error)
	lastInlineTaskPruneUnixMs    atomic.Int64
}

func NewRegistryService(
	store *registry.Store,
	initialCredentials map[string]string,
	heartbeatIntervalSec int32,
	offlineTTLSec int32,
	replayWindow time.Duration,
) *RegistryService {
	_ = replayWindow
	credentialCopy := make(map[string]string, len(initialCredentials))
	for workerID, secret := range initialCredentials {
		credentialCopy[workerID] = secret
	}
	return &RegistryService{
		store:                   store,
		credentials:             credentialCopy,
		credentialHashAlgo:      "legacy-plain",
		heartbeatIntervalSec:    heartbeatIntervalSec,
		offlineTTLSec:           offlineTTLSec,
		nowFn:                   time.Now,
		newSessionIDFn:          generateUUIDv4,
		newCommandIDFn:          generateUUIDv4,
		newTaskIDFn:             generateUUIDv4,
		newTerminalSessionIDFn:  generateUUIDv4,
		taskRetention:           defaultTaskRetentionWindow,
		sessions:                make(map[string]*activeSession),
		tasks:                   make(map[string]*taskRecord),
		taskRequestReservations: make(map[string]struct{}),
		criticalPersistenceFailureFn: func(err error) {
			panic(err)
		},
	}
}

func (s *RegistryService) SetTaskRetention(retention time.Duration) {
	if s == nil || retention <= 0 {
		return
	}
	s.tasksMu.Lock()
	s.taskRetention = retention
	s.tasksMu.Unlock()
}

func (s *RegistryService) PruneExpiredTasks(now time.Time) int {
	if s == nil || s.store == nil || s.store.Persistence() == nil {
		return 0
	}
	removed, err := s.store.Persistence().Queries.DeleteExpiredTerminalTasks(context.Background(), now.UnixMilli())
	if err != nil {
		return 0
	}
	return int(removed)
}
