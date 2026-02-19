package persistence

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

func TestOpenRunsMigrationAndStartupRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "console.db")
	now := time.Now()
	nowMS := now.UnixMilli()

	first, err := Open(ctx, Options{
		Path:             path,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		t.Fatalf("open first db: %v", err)
	}

	if err := first.Queries.UpsertWorkerNode(ctx, sqlc.UpsertWorkerNodeParams{
		NodeID:             "node-1",
		SessionID:          "session-1",
		Provisioned:        1,
		NodeName:           "node-1",
		ExecutorKind:       "docker",
		Version:            "v1",
		RegisteredAtUnixMs: nowMS,
		LastSeenAtUnixMs:   nowMS,
	}); err != nil {
		t.Fatalf("seed worker node: %v", err)
	}

	if err := first.Queries.InsertTask(ctx, sqlc.InsertTaskParams{
		TaskID:            "task-1",
		OwnerID:           "owner-1",
		RequestID:         "req-1",
		Capability:        "echo",
		InputJson:         `{"message":"hello"}`,
		Status:            "running",
		CommandID:         "cmd-1",
		ResultJson:        "",
		ErrorCode:         "",
		ErrorMessage:      "",
		CreatedAtUnixMs:   nowMS,
		UpdatedAtUnixMs:   nowMS,
		DeadlineAtUnixMs:  now.Add(1 * time.Minute).UnixMilli(),
		CompletedAtUnixMs: 0,
		ExpiresAtUnixMs:   0,
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first db: %v", err)
	}

	second, err := Open(ctx, Options{
		Path:             path,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		t.Fatalf("open second db: %v", err)
	}
	defer func() {
		_ = second.Close()
	}()

	worker, err := second.Queries.GetWorkerNodeByID(ctx, "node-1")
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if worker.SessionID != "" {
		t.Fatalf("expected session cleared on startup recovery, got %q", worker.SessionID)
	}

	task, err := second.Queries.GetTaskByID(ctx, "task-1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != "failed" {
		t.Fatalf("expected task status failed after recovery, got %q", task.Status)
	}
	if task.ErrorCode != "console_restarted" {
		t.Fatalf("expected error_code console_restarted, got %q", task.ErrorCode)
	}
	if task.CompletedAtUnixMs == 0 {
		t.Fatalf("expected completed_at_unix_ms to be set")
	}
	if task.ExpiresAtUnixMs <= nowMS {
		t.Fatalf("expected expires_at_unix_ms updated to future value")
	}
}

func TestDashboardCredentialTableSupportsDeleteAndReinsert(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "console-dashboard.db")
	nowMS := time.Now().UnixMilli()

	db, err := Open(ctx, Options{
		Path:             path,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()

	insert := func(username string, secret string) {
		t.Helper()
		err := db.Queries.InsertDashboardCredential(ctx, sqlc.InsertDashboardCredentialParams{
			SingletonID:     1,
			Username:        username,
			PasswordHash:    db.Hasher.Hash(secret),
			HashAlgo:        HashAlgorithmHMACSHA256,
			CreatedAtUnixMs: nowMS,
			UpdatedAtUnixMs: nowMS,
		})
		if err != nil {
			t.Fatalf("insert dashboard credential: %v", err)
		}
	}

	insert("admin-a", "secret-a")

	deleted, err := db.Queries.DeleteDashboardCredential(ctx)
	if err != nil {
		t.Fatalf("delete dashboard credential: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 row deleted, got %d", deleted)
	}

	_, err = db.Queries.GetDashboardCredential(ctx)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows after delete, got %v", err)
	}

	insert("admin-b", "secret-b")

	stored, err := db.Queries.GetDashboardCredential(ctx)
	if err != nil {
		t.Fatalf("get dashboard credential after reinsert: %v", err)
	}
	if stored.Username != "admin-b" {
		t.Fatalf("unexpected username after reinsert: %q", stored.Username)
	}
}
