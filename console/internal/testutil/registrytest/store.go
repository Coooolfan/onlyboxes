package registrytest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

func NewStore(t testing.TB) *registry.Store {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	path := fmt.Sprintf("file:onlyboxes-registry-test-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := persistence.Open(ctx, persistence.Options{
		Path:             path,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		t.Fatalf("open test registry db: %v", err)
	}
	return registry.NewStoreWithPersistence(db)
}
