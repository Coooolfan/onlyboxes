package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

type snapshotHTTPFixture struct {
	handler    *WorkerHandler
	router     *gin.Engine
	cookie     *http.Cookie
	repository *sandboxsnapshot.Repository
	service    *grpcserver.RegistryService
	objects    *objectstore.SnapshotStore
	store      *registry.Store
	operation  sandboxsnapshot.Operation
}

func newSnapshotHTTPFixture(t *testing.T, suspended bool) *snapshotHTTPFixture {
	t.Helper()
	ctx := context.Background()
	store := registrytest.NewStore(t)
	now := time.Now()
	scope := sandboxsnapshot.ScopeSessionID(testDashboardAccountID, "persistent-session")
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{ScopedSessionID: scope, NodeID: "source-worker", LeaseExpiresUnixMs: now.Add(time.Hour).UnixMilli(), CreatedAtUnixMs: now.UnixMilli(), LastUsedUnixMs: now.UnixMilli(), UpdatedAtUnixMs: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	repo := sandboxsnapshot.NewRepository(store.Persistence().SQL)
	spec := sandboxsnapshot.RestoreSpec{Runtime: "docker", RuntimeVersion: "29.4.0", Platform: "linux/arm64", Format: "docker-rootfs-v1", Image: "private-image@sha256:" + strings.Repeat("a", 64), MemoryMiB: 256, CPUs: 1, MaxProcesses: 128, Env: map[string]string{"PRIVATE": "restore-secret-do-not-publish"}}
	if err := repo.AdoptRunning(ctx, testDashboardAccountID, "persistent-session", "source-worker", spec); err != nil {
		t.Fatal(err)
	}
	svc := grpcserver.NewRegistryService(store, nil, 5, 15, time.Minute)
	if err := svc.RestoreTerminalSessionRoutes(ctx, now); err != nil {
		t.Fatal(err)
	}
	objects, err := objectstore.NewSnapshotStore(objectstore.SnapshotConfig{Config: objectstore.Config{Endpoint: "https://snapshot.invalid", Region: "test", BucketName: "snapshots", AccessKey: "private-ak", SecretKey: "private-sk"}, Prefix: "private-snapshot-prefix", MaxBytes: 16 << 20, PartSize: 5 << 20, PresignTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	f := &snapshotHTTPFixture{repository: repo, service: svc, objects: objects, store: store}
	if suspended {
		op, err := repo.Begin(ctx, sandboxsnapshot.BeginRequest{OwnerID: testDashboardAccountID, SessionID: "persistent-session", Kind: sandboxsnapshot.Suspend, IdempotencyKey: "seed-suspend", Parameters: json.RawMessage(`{"retention_ttl_sec":3600}`), DeadlineUnixMS: now.Add(time.Hour).UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		f.operation = op
		digest := sha256.Sum256([]byte("snapshot"))
		object, err := objects.Object(testDashboardAccountID, "persistent-session", "snapshot-id", 8, hex.EncodeToString(digest[:]))
		if err != nil {
			t.Fatal(err)
		}
		upload := objectstore.SnapshotUpload{Object: object, UploadID: "private-upload-id", PartSize: 5 << 20, ExpandedBytes: 8}
		if err := repo.ReserveUpload(ctx, op, upload, 100<<30); err != nil {
			t.Fatal(err)
		}
		if err := repo.RecordPart(ctx, op, "snapshot-id", objectstore.SnapshotPart{Number: 1, ETag: "private-etag", Size: 8}); err != nil {
			t.Fatal(err)
		}
		if err := repo.MarkVerified(ctx, op, "snapshot-id", sandboxsnapshot.Manifest{Version: 1, Archive: object, RestoreSpec: spec, ExpandedBytes: 8}, ""); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitSuspend(ctx, op, "snapshot-id", time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := repo.Finish(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	f.handler = NewWorkerHandler(store, 15*time.Second, svc, svc, svc, "")
	f.handler.SetTerminalSessionRegistry(svc)
	f.router = mustNewRouter(t, f.handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	f.cookie = loginSessionCookie(t, f.router)
	return f
}
func (f *snapshotHTTPFixture) request(method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(f.cookie)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, req)
	return response
}

const snapshotHTTPPath = "/api/v1/sessions/persistent-session"
const snapshotHTTPAccount = "?account_id=" + testDashboardAccountID

func TestSnapshotHTTPStableViewsNullRuntimeBindingAndLogicalDelete(t *testing.T) {
	f := newSnapshotHTTPFixture(t, true)
	for _, path := range []string{snapshotHTTPPath + snapshotHTTPAccount, "/api/v1/sessions" + snapshotHTTPAccount} {
		response := f.request(http.MethodGet, path, "", "")
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String())
		}
		for _, expected := range []string{`"lifecycle_state":"suspended"`, `"worker_id":null`, `"status":null`, `"lease_expires_at":null`, `"enabled":true`, `"snapshot_id":"snapshot-id"`, `"current_operation_id":null`} {
			if !strings.Contains(response.Body.String(), expected) {
				t.Fatal("missing lifecycle field", expected, response.Body.String())
			}
		}
		for _, private := range []string{"restore-secret-do-not-publish", "private-image", "private-snapshot-prefix", "private-upload-id", "private-etag", "private-ak", "snapshot.invalid"} {
			if strings.Contains(response.Body.String(), private) {
				t.Fatal("private restore/storage metadata leaked", private)
			}
		}
	}
	if response := f.request(http.MethodGet, snapshotHTTPPath+"?account_id=another-account", "", ""); response.Code != 404 {
		t.Fatal("cross-account lookup", response.Code)
	}
	if response := f.request(http.MethodPost, snapshotHTTPPath+"/renew"+snapshotHTTPAccount, `{"lease_ttl_sec":60}`, ""); response.Code != 409 {
		t.Fatal("suspended renewal", response.Code, response.Body.String())
	}
	// Logical deletion must work with new operations disabled, preserve the
	// archive reference until native cleanup and durably retain cleanup work.
	if response := f.request(http.MethodDelete, snapshotHTTPPath+snapshotHTTPAccount, "", ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := f.request(http.MethodGet, snapshotHTTPPath+snapshotHTTPAccount, "", ""); response.Code != 404 {
		t.Fatal("deleted stable session was exposed")
	}
	if response := f.request(http.MethodGet, snapshotHTTPPath+"/operations/"+f.operation.ID+snapshotHTTPAccount, "", ""); response.Code != 404 {
		t.Fatal("deleted session operation was exposed")
	}

	statsResponse := f.request(http.MethodGet, "/api/v1/persistence/stats"+snapshotHTTPAccount, "", "")
	if statsResponse.Code != 200 || !strings.Contains(statsResponse.Body.String(), `"pending_sessions":1`) || !strings.Contains(statsResponse.Body.String(), `"pending_workers":1`) || !strings.Contains(statsResponse.Body.String(), `"pending_snapshots":1`) {
		t.Fatal("cleanup backlog was not visible", statsResponse.Code, statsResponse.Body.String())
	}
	otherStats := f.request(http.MethodGet, "/api/v1/persistence/stats?account_id=another-account", "", "")
	if otherStats.Code != 200 || !strings.Contains(otherStats.Body.String(), `"pending_sessions":0`) {
		t.Fatal("cleanup stats crossed account scope", otherStats.Body.String())
	}
	var state, latest, job string
	if err := f.store.Persistence().SQL.QueryRow(`SELECT s.state,s.latest_snapshot_id,d.status FROM sandbox_persistent_sessions s JOIN sandbox_session_deletions d USING(owner_id,session_id)`).Scan(&state, &latest, &job); err != nil {
		t.Fatal(err)
	}
	if state != "deleting" || latest != "snapshot-id" || job != "pending" {
		t.Fatal("logical deletion lost cleanup/reference ownership", state, latest, job)
	}
	response := f.request(http.MethodGet, "/api/v1/sessions"+snapshotHTTPAccount, "", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "persistent-session") {
		t.Fatal("stale runtime route revived a deleted session", response.Body.String())
	}
}

func TestSnapshotHTTPResumeAsyncIdempotencyAndNoCompatibleWorker(t *testing.T) {
	f := newSnapshotHTTPFixture(t, true)
	coordinator, err := grpcserver.NewSnapshotCoordinator(f.service, f.objects, grpcserver.SnapshotCoordinatorConfig{MaxBytes: 16 << 20, MaxExpandedBytes: 64 << 20, AccountQuotaBytes: 100 << 30, OperationTimeout: 2 * time.Second, RetentionTTL: time.Hour, MaxRetentionTTL: 24 * time.Hour, RetryInterval: 10 * time.Millisecond, ReconcileTimeout: 100 * time.Millisecond, DrainTimeout: time.Second, MultipartConcurrency: 1, MaxConcurrentOperations: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := coordinator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		stop, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		if err := coordinator.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	f.handler.SetSnapshotCoordinator(coordinator, true)
	response := f.request(http.MethodPost, snapshotHTTPPath+"/resume"+snapshotHTTPAccount, `{"lease_ttl_sec":60}`, "resume-key")
	if response.Code != 202 || response.Header().Get("Location") == "" {
		t.Fatal("resume was not asynchronous", response.Code, response.Body.String())
	}
	var accepted snapshotOperationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	location := response.Header().Get("Location")
	deadline := time.Now().Add(3 * time.Second)
	for {
		response = f.request(http.MethodGet, location, "", "")
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		var op snapshotOperationResponse
		if err := json.Unmarshal(response.Body.Bytes(), &op); err != nil {
			t.Fatal(err)
		}
		if op.Status == "failed" {
			if op.Error == nil || op.Error.Code != "no_worker" || !op.Error.Retryable {
				t.Fatal("no-worker contract changed", response.Body.String())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async operation did not settle", response.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	response = f.request(http.MethodPost, snapshotHTTPPath+"/resume"+snapshotHTTPAccount, `{"lease_ttl_sec":60}`, "resume-key")
	var repeated snapshotOperationResponse
	_ = json.Unmarshal(response.Body.Bytes(), &repeated)
	if response.Code != 202 || repeated.ID != accepted.ID {
		t.Fatal("idempotent retry created another operation", response.Code, response.Body.String())
	}
	response = f.request(http.MethodPost, snapshotHTTPPath+"/resume"+snapshotHTTPAccount, `{"lease_ttl_sec":61}`, "resume-key")
	if response.Code != 409 {
		t.Fatal("same key with different parameters accepted", response.Code)
	}
	response = f.request(http.MethodGet, snapshotHTTPPath+snapshotHTTPAccount, "", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"lifecycle_state":"suspended"`) || !strings.Contains(response.Body.String(), `"snapshot_id":"snapshot-id"`) {
		t.Fatal("failed restore consumed the snapshot", response.Body.String())
	}
	f.handler.SetSnapshotCoordinator(coordinator, false)
	if response := f.request(http.MethodPost, snapshotHTTPPath+"/resume"+snapshotHTTPAccount, `{"lease_ttl_sec":60}`, "disabled-key"); response.Code != 503 {
		t.Fatal("disabled feature accepted resume", response.Code)
	}
	if response := f.request(http.MethodGet, location, "", ""); response.Code != 200 {
		t.Fatal("disabled feature hid existing operations")
	}
}

func TestSnapshotHTTPRequestValidationAndManagementAuth(t *testing.T) {
	f := newSnapshotHTTPFixture(t, true)
	cases := []struct{ action, body, key string }{
		{"suspend", `{}`, ""}, {"suspend", `null`, "key"}, {"suspend", `{"retention_ttl_sec":null}`, "key"}, {"suspend", `{"retention_ttl_sec":0}`, "key"}, {"suspend", `{"retention_ttl_sec":9223372036854775807}`, "key"},
		{"suspend", `{"worker_id":"untrusted"}`, "key"}, {"suspend", `{} {}`, "key"}, {"resume", `{}`, "key"}, {"resume", `{"lease_ttl_sec":1.5}`, "key"}, {"resume", `{"lease_ttl_sec":-1}`, "key"}, {"resume", `{"lease_ttl_sec":60,"archive_url":"secret-url"}`, "key"},
	}
	for _, tc := range cases {
		response := f.request(http.MethodPost, snapshotHTTPPath+"/"+tc.action+snapshotHTTPAccount, tc.body, tc.key)
		if response.Code != 400 || strings.Contains(response.Body.String(), "secret-url") {
			t.Fatal("unsafe lifecycle payload accepted or reflected", tc, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{snapshotHTTPPath + "/resume" + snapshotHTTPAccount, snapshotHTTPPath + "/suspend" + snapshotHTTPAccount, snapshotHTTPPath + "/operations/" + f.operation.ID + snapshotHTTPAccount} {
		method := http.MethodPost
		if strings.Contains(path, "/operations/") {
			method = http.MethodGet
		}
		req := httptest.NewRequest(method, path, strings.NewReader(`{"lease_ttl_sec":60}`))
		setMCPTokenHeader(req)
		response := httptest.NewRecorder()
		f.router.ServeHTTP(response, req)
		if response.Code != 401 {
			t.Fatal("execution token authorized lifecycle management", response.Code)
		}
	}
	if response := f.request(http.MethodPost, snapshotHTTPPath+"/resume", `{"lease_ttl_sec":60}`, "key"); response.Code != 400 {
		t.Fatal("admin owner selection was optional", response.Code)
	}
}

func TestSnapshotHTTPManagementKeysJITAndAccountIsolation(t *testing.T) {
	f := newSnapshotHTTPFixture(t, true)
	bundle := newTestAuthBundle(t, false)
	router := mustNewRouter(t, f.handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)
	key, err := bundle.APIKeyAuth.createAPIKey(context.Background(), testDashboardAccountID, "snapshot-management", "")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	location := snapshotHTTPPath + "/operations/" + f.operation.ID
	if response := request(http.MethodGet, location+snapshotHTTPAccount, key.Key, nil); response.Code != 200 {
		t.Fatal("API key cannot poll its operation", response.Code, response.Body.String())
	}
	memberID := "snapshot-member"
	seedTestAccount(t, bundle.ConsoleAuth.queries, memberID, "snapshot-member", "member-password", false)
	memberCookie := loginSessionCookieFor(t, router, "snapshot-member", "member-password")
	// Both accounts own the same external session ID. Only the account prefix
	// distinguishes the two stable identities and their operation history.
	own, err := f.repository.Session(context.Background(), testDashboardAccountID, "persistent-session")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := f.store.UpsertConfirmedTerminalSessionRoute(context.Background(), registry.TerminalSessionRoute{ScopedSessionID: sandboxsnapshot.ScopeSessionID(memberID, "persistent-session"), NodeID: "member-worker", LeaseExpiresUnixMs: now.Add(time.Hour).UnixMilli(), CreatedAtUnixMs: now.UnixMilli(), LastUsedUnixMs: now.UnixMilli(), UpdatedAtUnixMs: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := f.repository.AdoptRunning(context.Background(), memberID, "persistent-session", "member-worker", own.Spec); err != nil {
		t.Fatal(err)
	}
	if response := request(http.MethodGet, snapshotHTTPPath, "", memberCookie); response.Code != 200 || !strings.Contains(response.Body.String(), `"account_id":"snapshot-member"`) || strings.Contains(response.Body.String(), `"snapshot_id":"snapshot-id"`) {
		t.Fatal("member session lookup crossed account boundary", response.Code, response.Body.String())
	}
	for _, tc := range []struct{ method, path string }{{http.MethodGet, location}, {http.MethodGet, snapshotHTTPPath + snapshotHTTPAccount}, {http.MethodDelete, snapshotHTTPPath + snapshotHTTPAccount}} {
		if response := request(tc.method, tc.path, "", memberCookie); response.Code != 404 {
			t.Fatal("member accessed another account", tc, response.Code)
		}
	}
	jit := makeTestDashboardJITToken(t, "snapshot-issuer", "snapshot-subject")
	if response := request(http.MethodGet, "/api/v1/sessions", jit, nil); response.Code != 200 {
		t.Fatal("dashboard JIT rejected by lifecycle management", response.Code, response.Body.String())
	}
	if response := request(http.MethodGet, location+snapshotHTTPAccount, jit, nil); response.Code != 404 {
		t.Fatal("dashboard JIT crossed its account boundary", response.Code, response.Body.String())
	}
}

func TestSnapshotOperationResponseSanitizesWorkerErrors(t *testing.T) {
	for _, code := range []string{"snapshot_storage_error", "private-signed-token"} {
		response := operationResponse(sandboxsnapshot.Operation{ID: "operation", SessionID: "session", ErrorCode: code, ErrorMessage: "https://private.invalid?X-Amz-Signature=secret", Parameters: json.RawMessage(`{"private":"restore-env-secret"}`)})
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private.invalid", "X-Amz", "restore-env-secret", "private-signed-token"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatal("operation response leaked private error/configuration", secret)
			}
		}
	}
}
