package grpcserver

import (
	"context"
	"errors"
	"net/netip"
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
	defaultTerminalRouteTTL       = 30 * time.Minute
	terminalRoutePruneMinInterval = 1 * time.Minute
	terminalRouteStoreTimeout     = 5 * time.Second
	computerUseCapabilityName     = "computeruse"
	computerUseCapabilityDeclared = "computerUse"
	readImageCapabilityName       = "readimage"
	readImageCapabilityDeclared   = "readImage"
)

// workerSysAllowedCapabilities is the complete capability surface that a
// provisioned worker-sys may advertise. Values are the canonical wire names.
var workerSysAllowedCapabilities = map[string]string{
	computerUseCapabilityName: computerUseCapabilityDeclared,
	readImageCapabilityName:   readImageCapabilityDeclared,
}

var ErrNoEchoWorker = errors.New("no online worker supports echo")
var ErrEchoTimeout = errors.New("echo command timed out")
var ErrNoCapabilityWorker = errors.New("no online worker supports capability")
var ErrNoWorkerCapacity = errors.New("no online worker capacity for capability")
var ErrTaskRequestInProgress = errors.New("task request already in progress")

type RegistryService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer

	store                     *registry.Store
	credentialsMu             sync.RWMutex
	credentials               map[string]string
	credentialHashAlgo        string
	hasher                    *persistence.Hasher
	heartbeatIntervalSec      int32
	offlineTTLSec             int32
	nowFn                     func() time.Time
	newSessionIDFn            func() (string, error)
	newCommandIDFn            func() (string, error)
	newTaskIDFn               func() (string, error)
	newTerminalSessionIDFn    func() (string, error)
	taskRetention             time.Duration
	proxyEnabled              bool
	proxyAllowedWorkerCIDRs   []netip.Prefix
	proxyAllowedWorkerPorts   []uint16
	proxyAllowedDirectDomains []string

	sessionsMu sync.RWMutex
	sessions   map[string]*activeSession
	roundRobin uint64

	terminalRoutesMu             sync.RWMutex
	terminalSessionToNode        map[string]terminalSessionRoute
	terminalNodeToSessionIDIndex map[string]map[string]struct{}
	terminalRouteReservationSeq  uint64
	terminalRouteTTL             time.Duration
	terminalRouteStore           terminalSessionRouteStore
	proxyRouteSessionRevoker     ProxyRouteSessionRevoker
	lastTerminalRoutePruneUnixMs atomic.Int64

	tasksMu sync.RWMutex
	// Active task runtime index:
	// - tasks stores in-flight task runtime records by task_id.
	// - taskRequestReservations tracks (owner_id, request_id) currently being validated/created.
	tasks                        map[string]*taskRecord
	taskRequestReservations      map[string]struct{}
	criticalPersistenceFailureFn func(error)
	lastInlineTaskPruneUnixMs    atomic.Int64
}

type ProxyRouteSessionRevoker interface {
	RevokeSessionRoutes(...string) int
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
		store:                        store,
		credentials:                  credentialCopy,
		credentialHashAlgo:           "legacy-plain",
		heartbeatIntervalSec:         heartbeatIntervalSec,
		offlineTTLSec:                offlineTTLSec,
		nowFn:                        time.Now,
		newSessionIDFn:               generateUUIDv4,
		newCommandIDFn:               generateUUIDv4,
		newTaskIDFn:                  generateUUIDv4,
		newTerminalSessionIDFn:       generateUUIDv4,
		taskRetention:                defaultTaskRetentionWindow,
		sessions:                     make(map[string]*activeSession),
		terminalSessionToNode:        make(map[string]terminalSessionRoute),
		terminalNodeToSessionIDIndex: make(map[string]map[string]struct{}),
		terminalRouteTTL:             defaultTerminalRouteTTL,
		terminalRouteStore:           store,
		tasks:                        make(map[string]*taskRecord),
		taskRequestReservations:      make(map[string]struct{}),
		criticalPersistenceFailureFn: func(error) {},
	}
}

func (s *RegistryService) SetTaskRetention(retention time.Duration) {
	if s == nil || retention <= 0 {
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	s.taskRetention = retention
}

func (s *RegistryService) SetProxyRouteSessionRevoker(revoker ProxyRouteSessionRevoker) {
	if s == nil {
		return
	}
	s.proxyRouteSessionRevoker = revoker
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
