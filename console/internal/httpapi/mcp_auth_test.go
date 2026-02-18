package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMCPAuthRequireTokenRejectsMissingHeader(t *testing.T) {
	auth := NewMCPAuth([]string{"token-a"})
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

func TestMCPAuthRequireTokenRejectsWrongToken(t *testing.T) {
	auth := NewMCPAuth([]string{"token-a"})
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(mcpTokenHeader, "token-b")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenAllowsWhitelistedToken(t *testing.T) {
	auth := NewMCPAuth([]string{"token-a"})
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
	req.Header.Set(mcpTokenHeader, "token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthRequireTokenRejectsEmptyWhitelist(t *testing.T) {
	auth := NewMCPAuth([]string{})
	router := gin.New()
	router.GET("/mcp", auth.RequireToken(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(mcpTokenHeader, "token-a")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPAuthListTokens(t *testing.T) {
	auth := NewMCPAuth([]string{"token-a", " token-b ", "token-a", "  "})
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/console/mcp/tokens", nil)

	auth.ListTokens(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var payload mcpTokensResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if payload.Total != 2 {
		t.Fatalf("expected total=2, got %d", payload.Total)
	}
	if len(payload.Tokens) != 2 || payload.Tokens[0] != "token-a" || payload.Tokens[1] != "token-b" {
		t.Fatalf("unexpected tokens payload: %#v", payload.Tokens)
	}
}
