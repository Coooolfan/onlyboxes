package persistence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
	"github.com/pressly/goose/v3"
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

func TestAccountsTableSupportsInsertAndLookup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "console-accounts.db")
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

	err = db.Queries.InsertAccount(ctx, sqlc.InsertAccountParams{
		AccountID:       "acc-test-admin",
		Username:        "admin-test",
		UsernameKey:     "admin-test",
		PasswordHash:    db.Hasher.Hash("secret"),
		HashAlgo:        HashAlgorithmHMACSHA256,
		IsAdmin:         1,
		CreatedAtUnixMs: nowMS,
		UpdatedAtUnixMs: nowMS,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}

	adminCount, err := db.Queries.CountAdminAccounts(ctx)
	if err != nil {
		t.Fatalf("count admin accounts: %v", err)
	}
	if adminCount != 1 {
		t.Fatalf("expected 1 admin account, got %d", adminCount)
	}

	stored, err := db.Queries.GetAccountByUsernameKey(ctx, "admin-test")
	if err != nil {
		t.Fatalf("get account by username key: %v", err)
	}
	if stored.AccountID != "acc-test-admin" {
		t.Fatalf("unexpected account id: %q", stored.AccountID)
	}
	if stored.IsAdmin != 1 {
		t.Fatalf("expected is_admin=1, got %d", stored.IsAdmin)
	}
}

func TestOpenCreatesParentDirectoryWhenMissing(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nested", "db", "console.db")
	parentDir := filepath.Dir(dbPath)

	if _, err := os.Stat(parentDir); !os.IsNotExist(err) {
		t.Fatalf("expected parent directory to not exist before open")
	}

	db, err := Open(ctx, Options{
		Path:             dbPath,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		t.Fatalf("open db with missing parent directory: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()

	info, err := os.Stat(parentDir)
	if err != nil {
		t.Fatalf("stat parent directory after open: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %q to be a directory", parentDir)
	}
}

func TestMultipleWorkerSysMigrationRejectsRollbackWithDuplicateOwner(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{
		Path:             filepath.Join(t.TempDir(), "console-rollback.db"),
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

	for _, nodeID := range []string{"worker-sys-1", "worker-sys-2"} {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO worker_nodes (
				node_id, registered_at_unix_ms, last_seen_at_unix_ms
			) VALUES (?, 1, 1)
		`, nodeID); err != nil {
			t.Fatalf("insert worker node %q: %v", nodeID, err)
		}
		for key, value := range map[string]string{
			"obx.owner_id":    "owner-1",
			"obx.worker_type": "worker-sys",
		} {
			if _, err := db.SQL.ExecContext(ctx, `
				INSERT INTO worker_labels (node_id, label_key, label_value)
				VALUES (?, ?, ?)
			`, nodeID, key, value); err != nil {
				t.Fatalf("insert worker label %q for %q: %v", key, nodeID, err)
			}
		}
	}

	rollbackErr := goose.DownToContext(ctx, db.SQL, ".", 8)
	if rollbackErr == nil {
		t.Fatal("expected migration rollback to reject duplicate worker-sys owner")
	}
	if !strings.Contains(rollbackErr.Error(), "rollback_requires_at_most_one_worker_sys_per_owner") {
		t.Fatalf("expected actionable rollback error, got %v", rollbackErr)
	}

	version, err := goose.GetDBVersionContext(ctx, db.SQL)
	if err != nil {
		t.Fatalf("get migration version: %v", err)
	}
	if version != 9 {
		t.Fatalf("expected failed rollback to retain version 9, got %d", version)
	}

	var claimTableCount int
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND name = 'worker_sys_owner_claims'
	`).Scan(&claimTableCount); err != nil {
		t.Fatalf("inspect worker_sys_owner_claims table: %v", err)
	}
	if claimTableCount != 0 {
		t.Fatal("failed rollback must not recreate worker_sys_owner_claims")
	}
}
