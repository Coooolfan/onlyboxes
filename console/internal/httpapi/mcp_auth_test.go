package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestNewMCPAuthWithPersistenceNilReturnsError(t *testing.T) {
	auth, err := NewMCPAuthWithPersistence(nil)
	if !errors.Is(err, ErrMCPPersistenceDBRequired) {
		t.Fatalf("expected ErrMCPPersistenceDBRequired, got %v", err)
	}
	if auth != nil {
		t.Fatalf("expected nil auth when persistence db is nil")
	}
}

func TestMCPAuthRequireTokenRejectsMissingHeader(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	token := "token-a"
	if _, _, err := auth.createToken(context.Background(), testDashboardAccountID, "token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenRejectsLegacyTokenHeader(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	token := "token-a"
	if _, _, err := auth.createToken(context.Background(), testDashboardAccountID, "token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("X-Onlyboxes-Token", "token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenRejectsNonBearerAuthorization(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	token := "token-a"
	if _, _, err := auth.createToken(context.Background(), testDashboardAccountID, "token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "Basic token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenRejectsBearerWithoutToken(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	token := "token-a"
	if _, _, err := auth.createToken(context.Background(), testDashboardAccountID, "token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "Bearer")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenRejectsWrongToken(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	token := "token-a"
	if _, _, err := auth.createToken(context.Background(), testDashboardAccountID, "token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "Bearer token-b")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenAllowsTrustedToken(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	token := "token-a"
	if _, _, err := auth.createToken(context.Background(), testDashboardAccountID, "token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		if got := requestOwnerIDFromGin(c); got != testDashboardAccountID {
			t.Fatalf("expected owner id in gin context=%q, got %q", testDashboardAccountID, got)
		}
		if got := requestOwnerIDFromContext(c.Request.Context()); got != testDashboardAccountID {
			t.Fatalf("expected owner id in request context=%q, got %q", testDashboardAccountID, got)
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "Bearer token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenAllowsJITTokenAndCreatesAccount(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	jitToken := makeTestJITToken(t, "Acme Platform", "User 42")
	expectedIdentity, ok := deriveJITAccountIdentity(jitTokenClaims{
		Issuer:  "Acme Platform",
		Subject: "User 42",
	})
	if !ok {
		t.Fatalf("expected deterministic JIT identity")
	}

	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		if got := requestOwnerIDFromGin(c); got != expectedIdentity.AccountID {
			t.Fatalf("expected owner id in gin context=%q, got %q", expectedIdentity.AccountID, got)
		}
		if got := requestOwnerIDFromContext(c.Request.Context()); got != expectedIdentity.AccountID {
			t.Fatalf("expected owner id in request context=%q, got %q", expectedIdentity.AccountID, got)
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "Bearer "+jitToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	account, err := auth.queries.GetAccountByID(context.Background(), expectedIdentity.AccountID)
	if err != nil {
		t.Fatalf("expected JIT account to be created: %v", err)
	}
	if account.Username != expectedIdentity.Username {
		t.Fatalf("expected jit username %q, got %q", expectedIdentity.Username, account.Username)
	}
	if account.IsAdmin != 0 {
		t.Fatalf("expected non-admin JIT account, got is_admin=%d", account.IsAdmin)
	}
}

func TestMCPAuthRequireTokenRejectsInvalidJITToken(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	testCases := []struct {
		name  string
		token string
	}{
		{name: "invalid signature", token: makeTestJITToken(t, "issuer-a", "subject-a") + "broken"},
		{name: "invalid payload", token: jitTokenPrefix + "!not-base64!.sig"},
		{name: "missing subject", token: makeTestJITTokenWithClaims(t, jitTokenClaims{Issuer: "issuer-a"})},
		{name: "normalized issuer empty", token: makeTestJITToken(t, "!!!!", "subject-a")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			req.Header.Set(trustedTokenHeader, "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMCPAuthJITAccountDerivationIsStableAndScopedByIssuer(t *testing.T) {
	firstIdentity, ok := deriveJITAccountIdentity(jitTokenClaims{
		Issuer:  "issuer-a",
		Subject: "same-user",
	})
	if !ok {
		t.Fatalf("expected first JIT identity")
	}
	repeatedIdentity, ok := deriveJITAccountIdentity(jitTokenClaims{
		Issuer:  "issuer-a",
		Subject: "same-user",
	})
	if !ok {
		t.Fatalf("expected repeated JIT identity")
	}
	secondIdentity, ok := deriveJITAccountIdentity(jitTokenClaims{
		Issuer:  "issuer-b",
		Subject: "same-user",
	})
	if !ok {
		t.Fatalf("expected second JIT identity")
	}

	if firstIdentity.AccountID != repeatedIdentity.AccountID {
		t.Fatalf("expected identical account ids, got %q and %q", firstIdentity.AccountID, repeatedIdentity.AccountID)
	}
	if firstIdentity.Username != repeatedIdentity.Username {
		t.Fatalf("expected identical usernames, got %q and %q", firstIdentity.Username, repeatedIdentity.Username)
	}
	if firstIdentity.AccountID == secondIdentity.AccountID {
		t.Fatalf("expected issuer-scoped account ids to differ, got %q", firstIdentity.AccountID)
	}
}

func TestMCPAuthRequireTokenRejectsWhenStoreIsEmpty(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "Bearer token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthTokenCRUD(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	router := gin.New()
	router.Use(withTestSessionAccount(SessionAccount{AccountID: testDashboardAccountID, Username: testDashboardUsername, IsAdmin: true}))
	router.POST("/tokens", auth.CreateToken)
	router.GET("/tokens", auth.ListTokens)
	router.GET("/tokens/:token_id/value", auth.GetTokenValue)
	router.DELETE("/tokens/:token_id", auth.DeleteToken)

	createManualReq := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(`{"name":"CI Prod","token":"manual-token-1"}`))
	createManualReq.Header.Set("Content-Type", "application/json")
	createManualRec := httptest.NewRecorder()
	router.ServeHTTP(createManualRec, createManualReq)
	if createManualRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createManualRec.Code, createManualRec.Body.String())
	}
	manualPayload := createTrustedTokenResponse{}
	if err := json.Unmarshal(createManualRec.Body.Bytes(), &manualPayload); err != nil {
		t.Fatalf("decode manual create response: %v", err)
	}
	if manualPayload.Generated {
		t.Fatalf("expected generated=false for manual token")
	}
	if manualPayload.Token != "manual-token-1" {
		t.Fatalf("unexpected manual token: %q", manualPayload.Token)
	}
	if manualPayload.TokenMasked != "manu******en-1" {
		t.Fatalf("unexpected manual token mask: %q", manualPayload.TokenMasked)
	}

	createGeneratedReq := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(`{"name":"CI Generated"}`))
	createGeneratedReq.Header.Set("Content-Type", "application/json")
	createGeneratedRec := httptest.NewRecorder()
	router.ServeHTTP(createGeneratedRec, createGeneratedReq)
	if createGeneratedRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createGeneratedRec.Code, createGeneratedRec.Body.String())
	}
	generatedPayload := createTrustedTokenResponse{}
	if err := json.Unmarshal(createGeneratedRec.Body.Bytes(), &generatedPayload); err != nil {
		t.Fatalf("decode generated create response: %v", err)
	}
	if !generatedPayload.Generated {
		t.Fatalf("expected generated=true")
	}
	if !strings.HasPrefix(generatedPayload.Token, generatedTokenPrefix) {
		t.Fatalf("expected generated token prefix %q, got %q", generatedTokenPrefix, generatedPayload.Token)
	}
	if len(generatedPayload.Token) != len(generatedTokenPrefix)+generatedTokenByteLength*2 {
		t.Fatalf("unexpected generated token length: %d", len(generatedPayload.Token))
	}

	listReq := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", listRec.Code, listRec.Body.String())
	}
	listPayload := trustedTokenListResponse{}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if listPayload.Total != 2 || len(listPayload.Items) != 2 {
		t.Fatalf("expected 2 items, got total=%d len=%d", listPayload.Total, len(listPayload.Items))
	}

	getValueReq := httptest.NewRequest(http.MethodGet, "/tokens/"+manualPayload.ID+"/value", nil)
	getValueRec := httptest.NewRecorder()
	router.ServeHTTP(getValueRec, getValueReq)
	if getValueRec.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d body=%s", getValueRec.Code, getValueRec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/tokens/"+manualPayload.ID, nil)
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", deleteRec.Code, deleteRec.Body.String())
	}

	getDeletedReq := httptest.NewRequest(http.MethodGet, "/tokens/"+manualPayload.ID+"/value", nil)
	getDeletedRec := httptest.NewRecorder()
	router.ServeHTTP(getDeletedRec, getDeletedReq)
	if getDeletedRec.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d body=%s", getDeletedRec.Code, getDeletedRec.Body.String())
	}
}

func TestMCPAuthCreateTokenConflicts(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	router := gin.New()
	router.Use(withTestSessionAccount(SessionAccount{AccountID: testDashboardAccountID, Username: testDashboardUsername, IsAdmin: true}))
	router.POST("/tokens", auth.CreateToken)

	firstReq := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(`{"name":"CI Prod","token":"manual-token-1"}`))
	firstReq.Header.Set("Content-Type", "application/json")
	firstRec := httptest.NewRecorder()
	router.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("expected first create status 201, got %d body=%s", firstRec.Code, firstRec.Body.String())
	}

	dupNameReq := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(`{"name":"ci prod","token":"manual-token-2"}`))
	dupNameReq.Header.Set("Content-Type", "application/json")
	dupNameRec := httptest.NewRecorder()
	router.ServeHTTP(dupNameRec, dupNameReq)
	if dupNameRec.Code != http.StatusConflict {
		t.Fatalf("expected duplicate name status 409, got %d body=%s", dupNameRec.Code, dupNameRec.Body.String())
	}

	dupValueReq := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(`{"name":"CI Prod 2","token":"manual-token-1"}`))
	dupValueReq.Header.Set("Content-Type", "application/json")
	dupValueRec := httptest.NewRecorder()
	router.ServeHTTP(dupValueRec, dupValueReq)
	if dupValueRec.Code != http.StatusConflict {
		t.Fatalf("expected duplicate value status 409, got %d body=%s", dupValueRec.Code, dupValueRec.Body.String())
	}
}

func TestMCPAuthTokenIsolationByAccount(t *testing.T) {
	auth := newBareTestMCPAuth(t)
	secondAccount := SessionAccount{AccountID: "acc-test-member-b", Username: "member-b", IsAdmin: false}
	seedTestAccount(t, auth.queries, secondAccount.AccountID, secondAccount.Username, "member-b-password", false)

	routerAdmin := gin.New()
	routerAdmin.Use(withTestSessionAccount(SessionAccount{AccountID: testDashboardAccountID, Username: testDashboardUsername, IsAdmin: true}))
	routerAdmin.POST("/tokens", auth.CreateToken)
	routerAdmin.GET("/tokens", auth.ListTokens)
	routerAdmin.DELETE("/tokens/:token_id", auth.DeleteToken)

	routerMember := gin.New()
	routerMember.Use(withTestSessionAccount(secondAccount))
	routerMember.POST("/tokens", auth.CreateToken)
	routerMember.GET("/tokens", auth.ListTokens)
	routerMember.DELETE("/tokens/:token_id", auth.DeleteToken)

	adminCreateReq := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(`{"name":"shared-name","token":"admin-token"}`))
	adminCreateReq.Header.Set("Content-Type", "application/json")
	adminCreateRec := httptest.NewRecorder()
	routerAdmin.ServeHTTP(adminCreateRec, adminCreateReq)
	if adminCreateRec.Code != http.StatusCreated {
		t.Fatalf("admin create token expected 201, got %d body=%s", adminCreateRec.Code, adminCreateRec.Body.String())
	}
	adminPayload := createTrustedTokenResponse{}
	if err := json.Unmarshal(adminCreateRec.Body.Bytes(), &adminPayload); err != nil {
		t.Fatalf("decode admin create response: %v", err)
	}

	memberCreateReq := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(`{"name":"shared-name","token":"member-token"}`))
	memberCreateReq.Header.Set("Content-Type", "application/json")
	memberCreateRec := httptest.NewRecorder()
	routerMember.ServeHTTP(memberCreateRec, memberCreateReq)
	if memberCreateRec.Code != http.StatusCreated {
		t.Fatalf("member create token expected 201, got %d body=%s", memberCreateRec.Code, memberCreateRec.Body.String())
	}

	adminListReq := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	adminListRec := httptest.NewRecorder()
	routerAdmin.ServeHTTP(adminListRec, adminListReq)
	if adminListRec.Code != http.StatusOK {
		t.Fatalf("admin list expected 200, got %d", adminListRec.Code)
	}
	adminList := trustedTokenListResponse{}
	if err := json.Unmarshal(adminListRec.Body.Bytes(), &adminList); err != nil {
		t.Fatalf("decode admin list: %v", err)
	}
	if adminList.Total != 1 {
		t.Fatalf("expected admin to see 1 token, got %d", adminList.Total)
	}

	memberDeleteAdminTokenReq := httptest.NewRequest(http.MethodDelete, "/tokens/"+adminPayload.ID, nil)
	memberDeleteAdminTokenRec := httptest.NewRecorder()
	routerMember.ServeHTTP(memberDeleteAdminTokenRec, memberDeleteAdminTokenReq)
	if memberDeleteAdminTokenRec.Code != http.StatusNotFound {
		t.Fatalf("expected member delete admin token -> 404, got %d body=%s", memberDeleteAdminTokenRec.Code, memberDeleteAdminTokenRec.Body.String())
	}
}

func withTestSessionAccount(account SessionAccount) gin.HandlerFunc {
	return func(c *gin.Context) {
		setRequestSessionAccount(c, account)
		c.Next()
	}
}
