package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onlyboxes/onlyboxes/console/db/migrations"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

const (
	defaultDBPath         = "./db/onlyboxes-console.db"
	defaultBusyTimeoutMS  = 5000
	defaultTaskRetentionD = 30
)

type Options struct {
	Path             string
	BusyTimeoutMS    int
	HashKey          string
	TaskRetentionDay int
}

type DB struct {
	SQL           *sql.DB
	Queries       *sqlc.Queries
	Hasher        *Hasher
	TaskRetention time.Duration
}

var gooseSetupOnce sync.Once

func Open(ctx context.Context, opts Options) (*DB, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		path = defaultDBPath
	}
	busyTimeout := opts.BusyTimeoutMS
	if busyTimeout <= 0 {
		busyTimeout = defaultBusyTimeoutMS
	}
	taskRetentionDays := opts.TaskRetentionDay
	if taskRetentionDays <= 0 {
		taskRetentionDays = defaultTaskRetentionD
	}
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}

	hasher, err := NewHasher(opts.HashKey)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	cleanupOnErr := true
	defer func() {
		if cleanupOnErr {
			_ = db.Close()
		}
	}()

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := applyPragmas(ctx, db, busyTimeout); err != nil {
		return nil, err
	}

	if err := runMigrations(ctx, db); err != nil {
		return nil, err
	}

	queries := sqlc.New(db)
	nowMS := time.Now().UnixMilli()
	retentionMS := int64((time.Duration(taskRetentionDays) * 24 * time.Hour).Milliseconds())
	expiresMS := nowMS + retentionMS
	if _, err := queries.MarkNonTerminalTasksFailedOnStartup(ctx, sqlc.MarkNonTerminalTasksFailedOnStartupParams{
		UpdatedAtUnixMs:   nowMS,
		CompletedAtUnixMs: nowMS,
		ExpiresAtUnixMs:   expiresMS,
	}); err != nil {
		return nil, fmt.Errorf("startup recovery tasks: %w", err)
	}
	if _, err := queries.ClearAllWorkerSessions(ctx); err != nil {
		return nil, fmt.Errorf("startup recovery sessions: %w", err)
	}

	cleanupOnErr = false
	return &DB{
		SQL:           db,
		Queries:       queries,
		Hasher:        hasher,
		TaskRetention: time.Duration(taskRetentionDays) * 24 * time.Hour,
	}, nil
}

func applyPragmas(ctx context.Context, db *sql.DB, busyTimeoutMS int) error {
	statements := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS),
	}
	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply %q: %w", stmt, err)
		}
	}
	return nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	gooseSetupOnce.Do(func() {
		goose.SetBaseFS(migrations.Files)
		goose.SetVerbose(false)
	})
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

func ensureParentDir(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || trimmed == ":memory:" || strings.HasPrefix(trimmed, "file:") {
		return nil
	}
	parentDir := filepath.Dir(trimmed)
	if parentDir == "." || parentDir == "" {
		return nil
	}
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("create sqlite parent directory %q: %w", parentDir, err)
	}
	return nil
}

func (d *DB) Close() error {
	if d == nil || d.SQL == nil {
		return nil
	}
	return d.SQL.Close()
}

func (d *DB) WithTx(ctx context.Context, fn func(q *sqlc.Queries) error) error {
	if d == nil || d.SQL == nil {
		return fmt.Errorf("database is not initialized")
	}
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	qtx := d.Queries.WithTx(tx)
	if err := fn(qtx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
