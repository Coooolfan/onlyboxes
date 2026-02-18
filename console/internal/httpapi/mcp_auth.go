package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/tokenlist"
)

const mcpTokenHeader = "X-Onlyboxes-MCP-Token"

type mcpTokensResponse struct {
	Tokens []string `json:"tokens"`
	Total  int      `json:"total"`
}

type MCPAuth struct {
	allowedSet    map[string]struct{}
	orderedTokens []string
}

func NewMCPAuth(tokens []string) *MCPAuth {
	ordered := tokenlist.Normalize(tokens)
	allowed := make(map[string]struct{}, len(ordered))
	for _, token := range ordered {
		allowed[token] = struct{}{}
	}
	return &MCPAuth{
		allowedSet:    allowed,
		orderedTokens: ordered,
	}
}

func (a *MCPAuth) RequireToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimSpace(c.GetHeader(mcpTokenHeader))
		if token == "" || a == nil || !a.isAllowed(token) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing mcp token"})
			c.Abort()
			return
		}
		setRequestOwnerID(c, ownerIDFromToken(token))
		c.Next()
	}
}

func (a *MCPAuth) ListTokens(c *gin.Context) {
	tokens := []string{}
	if a != nil {
		tokens = append(tokens, a.orderedTokens...)
	}
	c.JSON(http.StatusOK, mcpTokensResponse{
		Tokens: tokens,
		Total:  len(tokens),
	})
}

func (a *MCPAuth) isAllowed(token string) bool {
	if a == nil || len(a.allowedSet) == 0 {
		return false
	}
	_, ok := a.allowedSet[token]
	return ok
}
