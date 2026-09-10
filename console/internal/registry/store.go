package registry

import (
	"errors"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

var ErrNodeNotFound = errors.New("worker node not found")
var ErrSessionMismatch = errors.New("worker session mismatch")
var ErrPersistenceDBRequired = errors.New("registry store requires non-nil persistence db")

type WorkerStatus string

type WorkerConnectionConflictPolicy string

const (
	StatusAll     WorkerStatus = "all"
	StatusOnline  WorkerStatus = "online"
	StatusOffline WorkerStatus = "offline"

	WorkerConnectionConflictPolicyReplace WorkerConnectionConflictPolicy = "REPLACE"
	WorkerConnectionConflictPolicyReject  WorkerConnectionConflictPolicy = "REJECT"
)

type CapabilityDeclaration struct {
	Name        string `json:"name"`
	MaxInflight int32  `json:"max_inflight"`
}

type Worker struct {
	NodeID       string
	SessionID    string
	Provisioned  bool
	NodeName     string
	ExecutorKind string
	Capabilities []CapabilityDeclaration
	Labels       map[string]string
	Version      string
	RegisteredAt time.Time
	LastSeenAt   time.Time
}

type WorkerView struct {
	Worker
	Status WorkerStatus
}

type WorkerStats struct {
	Total   int
	Online  int
	Offline int
	Stale   int
}

type ProvisionedWorker struct {
	NodeID string
	Labels map[string]string
}

type Store struct {
	db      *persistence.DB
	queries *sqlc.Queries
}

func NewStoreWithPersistence(db *persistence.DB) (*Store, error) {
	if db == nil {
		return nil, ErrPersistenceDBRequired
	}
	return &Store{db: db, queries: db.Queries}, nil
}

func (s *Store) Persistence() *persistence.DB {
	if s == nil {
		return nil
	}
	return s.db
}
