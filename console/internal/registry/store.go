package registry

import (
	"errors"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

var ErrNodeNotFound = errors.New("worker node not found")
var ErrSessionMismatch = errors.New("worker session mismatch")

type WorkerStatus string

const (
	StatusAll     WorkerStatus = "all"
	StatusOnline  WorkerStatus = "online"
	StatusOffline WorkerStatus = "offline"
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
	Slot   int
	NodeID string
	Labels map[string]string
}

type Store struct {
	db      *persistence.DB
	queries *sqlc.Queries
}

func NewStoreWithPersistence(db *persistence.DB) *Store {
	if db == nil {
		panic("registry store requires non-nil persistence db")
	}
	return &Store{db: db, queries: db.Queries}
}

func (s *Store) Persistence() *persistence.DB {
	if s == nil {
		return nil
	}
	return s.db
}
