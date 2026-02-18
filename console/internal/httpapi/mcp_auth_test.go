package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMCPAuthRequireTokenRejectsMissingHeader(t *testing.T) {
	auth := NewMCPAuth()
	token := "token-a"
	if _, _, err := auth.createToken("token-a", &token); err != nil {
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

func TestMCPAuthRequireTokenRejectsOldHeader(t *testing.T) {
	auth := NewMCPAuth()
	token := "token-a"
	if _, _, err := auth.createToken("token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("X-Onlyboxes-MCP-Token", "token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenRejectsWrongToken(t *testing.T) {
	auth := NewMCPAuth()
	token := "token-a"
	if _, _, err := auth.createToken("token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "token-b")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenAllowsTrustedToken(t *testing.T) {
	auth := NewMCPAuth()
	token := "token-a"
	if _, _, err := auth.createToken("token-a", &token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		if got := requestOwnerIDFromGin(c); got != ownerIDFromToken("token-a") {
			t.Fatalf("expected owner id in gin context, got %q", got)
		}
		if got := requestOwnerIDFromContext(c.Request.Context()); got != ownerIDFromToken("token-a") {
			t.Fatalf("expected owner id in request context, got %q", got)
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenRejectsWhenStoreIsEmpty(t *testing.T) {
	auth := NewMCPAuth()
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(trustedTokenHeader, "token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthTokenCRUD(t *testing.T) {
	auth := NewMCPAuth()
	router := gin.New()
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
	if getValueRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", getValueRec.Code, getValueRec.Body.String())
	}
	valuePayload := trustedTokenValueResponse{}
	if err := json.Unmarshal(getValueRec.Body.Bytes(), &valuePayload); err != nil {
		t.Fatalf("decode value response: %v", err)
	}
	if valuePayload.Token != "manual-token-1" {
		t.Fatalf("unexpected token value: %q", valuePayload.Token)
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
	if getDeletedRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", getDeletedRec.Code, getDeletedRec.Body.String())
	}
}

func TestMCPAuthCreateTokenConflicts(t *testing.T) {
	auth := NewMCPAuth()
	router := gin.New()
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
