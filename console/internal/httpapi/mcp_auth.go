package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

const trustedTokenHeader = "X-Onlyboxes-Token"

const (
	maxTrustedTokenNameRunes = 64
	maxTrustedTokenLength    = 256
	generatedTokenPrefix     = "obx_"
	generatedTokenByteLength = 32
	tokenIDPrefix            = "tok_"
	tokenIDByteLength        = 16
)

var (
	errTrustedTokenNameRequired     = errors.New("name is required")
	errTrustedTokenNameTooLong      = errors.New("name length must be <= 64")
	errTrustedTokenNameConflict     = errors.New("token name already exists")
	errTrustedTokenValueRequired    = errors.New("token is required")
	errTrustedTokenValueTooLong     = errors.New("token length must be <= 256")
	errTrustedTokenValueWhitespace  = errors.New("token must not contain whitespace")
	errTrustedTokenValueConflict    = errors.New("token already exists")
	errTrustedTokenNotFound         = errors.New("token not found")
	errTrustedTokenGenerateFailed   = errors.New("failed to generate token")
	errTrustedTokenIDGenerateFailed = errors.New("failed to generate token id")
)

type trustedTokenItem struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	TokenMasked string    `json:"token_masked"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type trustedTokenListResponse struct {
	Items []trustedTokenItem `json:"items"`
	Total int                `json:"total"`
}

type createTrustedTokenRequest struct {
	Name  string  `json:"name"`
	Token *string `json:"token,omitempty"`
}

type createTrustedTokenResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Token       string    `json:"token"`
	TokenMasked string    `json:"token_masked"`
	Generated   bool      `json:"generated"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type trustedTokenValueResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

type trustedTokenRecord struct {
	ID          string
	Name        string
	NameKey     string
	Token       string
	TokenHash   string
	TokenMasked string
	Generated   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type MCPAuth struct {
	db      *persistence.DB
	queries *sqlc.Queries
	hasher  *persistence.Hasher
	nowFn   func() time.Time
}

func NewMCPAuthWithPersistence(db *persistence.DB) *MCPAuth {
	if db == nil {
		panic("mcp auth requires non-nil persistence db")
	}
	auth := &MCPAuth{
		db:      db,
		queries: db.Queries,
		hasher:  db.Hasher,
		nowFn:   time.Now,
	}
	if auth.hasher != nil {
		setOwnerIDHashFunc(auth.hasher.Hash)
	}
	return auth
}

func (a *MCPAuth) RequireToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimSpace(c.GetHeader(trustedTokenHeader))
		if token == "" || a == nil || a.hasher == nil || !a.isAllowed(token) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
			c.Abort()
			return
		}
		setRequestOwnerID(c, a.hasher.Hash(token))
		c.Next()
	}
}

func (a *MCPAuth) ListTokens(c *gin.Context) {
	if a == nil || a.queries == nil {
		c.JSON(http.StatusOK, trustedTokenListResponse{Items: []trustedTokenItem{}, Total: 0})
		return
	}

	records, err := a.queries.ListTrustedTokens(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tokens"})
		return
	}

	items := make([]trustedTokenItem, 0, len(records))
	for _, record := range records {
		items = append(items, trustedTokenItem{
			ID:          record.TokenID,
			Name:        record.Name,
			TokenMasked: record.TokenMasked,
			CreatedAt:   time.UnixMilli(record.CreatedAtUnixMs),
			UpdatedAt:   time.UnixMilli(record.UpdatedAtUnixMs),
		})
	}

	c.JSON(http.StatusOK, trustedTokenListResponse{
		Items: items,
		Total: len(items),
	})
}

func (a *MCPAuth) isAllowed(token string) bool {
	if a == nil || a.queries == nil || a.hasher == nil {
		return false
	}
	tokenHash := a.hasher.Hash(token)
	_, err := a.queries.GetTrustedTokenByHash(contextBackground(), tokenHash)
	return err == nil
}

func (a *MCPAuth) CreateToken(c *gin.Context) {
	if a == nil || a.queries == nil || a.hasher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "token store is unavailable"})
		return
	}

	req := createTrustedTokenRequest{}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	record, generated, err := a.createToken(req.Name, req.Token)
	if err != nil {
		switch {
		case errors.Is(err, errTrustedTokenNameRequired),
			errors.Is(err, errTrustedTokenNameTooLong),
			errors.Is(err, errTrustedTokenValueRequired),
			errors.Is(err, errTrustedTokenValueTooLong),
			errors.Is(err, errTrustedTokenValueWhitespace):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, errTrustedTokenNameConflict),
			errors.Is(err, errTrustedTokenValueConflict):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create token"})
		}
		return
	}

	c.JSON(http.StatusCreated, createTrustedTokenResponse{
		ID:          record.ID,
		Name:        record.Name,
		Token:       record.Token,
		TokenMasked: record.TokenMasked,
		Generated:   generated,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
	})
}

func (a *MCPAuth) DeleteToken(c *gin.Context) {
	if a == nil || a.queries == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "token store is unavailable"})
		return
	}

	tokenID := strings.TrimSpace(c.Param("token_id"))
	if tokenID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token_id is required"})
		return
	}

	err := a.deleteToken(tokenID)
	if err != nil {
		if errors.Is(err, errTrustedTokenNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "token not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete token"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *MCPAuth) GetTokenValue(c *gin.Context) {
	tokenID := strings.TrimSpace(c.Param("token_id"))
	if tokenID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token_id is required"})
		return
	}
	c.JSON(http.StatusGone, gin.H{
		"error": "token value is only returned at creation time; delete and recreate the token to obtain a new value",
	})
}

func (a *MCPAuth) createToken(name string, tokenInput *string) (trustedTokenRecord, bool, error) {
	normalizedName, nameKey, err := normalizeTokenName(name)
	if err != nil {
		return trustedTokenRecord{}, false, err
	}

	generated := tokenInput == nil
	tokenValue := ""
	if tokenInput != nil {
		tokenValue, err = normalizeTokenValue(*tokenInput)
		if err != nil {
			return trustedTokenRecord{}, false, err
		}
	}

	for i := 0; i < 8; i++ {
		if generated {
			generatedToken, genErr := generateTrustedToken()
			if genErr != nil {
				return trustedTokenRecord{}, false, errTrustedTokenGenerateFailed
			}
			tokenValue = generatedToken
		}

		tokenID, idErr := generateTokenID()
		if idErr != nil {
			return trustedTokenRecord{}, false, errTrustedTokenIDGenerateFailed
		}
		now := time.Now()
		if a.nowFn != nil {
			now = a.nowFn()
		}

		tokenHash := a.hasher.Hash(tokenValue)
		record := trustedTokenRecord{
			ID:          tokenID,
			Name:        normalizedName,
			NameKey:     nameKey,
			Token:       tokenValue,
			TokenHash:   tokenHash,
			TokenMasked: maskToken(tokenValue),
			Generated:   generated,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		err = a.queries.InsertTrustedToken(contextBackground(), sqlc.InsertTrustedTokenParams{
			TokenID:         record.ID,
			Name:            record.Name,
			NameKey:         record.NameKey,
			TokenHash:       record.TokenHash,
			TokenMasked:     record.TokenMasked,
			Generated:       boolToInt64(record.Generated),
			CreatedAtUnixMs: record.CreatedAt.UnixMilli(),
			UpdatedAtUnixMs: record.UpdatedAt.UnixMilli(),
		})
		if err == nil {
			return record, generated, nil
		}

		errText := strings.ToLower(err.Error())
		switch {
		case strings.Contains(errText, "trusted_tokens.name_key"):
			return trustedTokenRecord{}, false, errTrustedTokenNameConflict
		case strings.Contains(errText, "trusted_tokens.token_hash"):
			if generated {
				continue
			}
			return trustedTokenRecord{}, false, errTrustedTokenValueConflict
		case strings.Contains(errText, "trusted_tokens.token_id"):
			continue
		default:
			return trustedTokenRecord{}, false, err
		}
	}

	if generated {
		return trustedTokenRecord{}, false, errTrustedTokenGenerateFailed
	}
	return trustedTokenRecord{}, false, errTrustedTokenValueConflict
}

func (a *MCPAuth) deleteToken(tokenID string) error {
	if a == nil || a.queries == nil {
		return errors.New("token store is unavailable")
	}
	rows, err := a.queries.DeleteTrustedTokenByID(contextBackground(), tokenID)
	if err != nil {
		return err
	}
	if rows == 0 {
		return errTrustedTokenNotFound
	}
	return nil
}

func normalizeTokenName(value string) (string, string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", "", errTrustedTokenNameRequired
	}
	if utf8.RuneCountInString(trimmed) > maxTrustedTokenNameRunes {
		return "", "", errTrustedTokenNameTooLong
	}
	return trimmed, strings.ToLower(trimmed), nil
}

func normalizeTokenValue(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errTrustedTokenValueRequired
	}
	if len(trimmed) > maxTrustedTokenLength {
		return "", errTrustedTokenValueTooLong
	}
	for _, r := range trimmed {
		if unicode.IsSpace(r) {
			return "", errTrustedTokenValueWhitespace
		}
	}
	return trimmed, nil
}

func generateTrustedToken() (string, error) {
	value, err := randomHexString(generatedTokenByteLength)
	if err != nil {
		return "", err
	}
	return generatedTokenPrefix + value, nil
}

func generateTokenID() (string, error) {
	value, err := randomHexString(tokenIDByteLength)
	if err != nil {
		return "", err
	}
	return tokenIDPrefix + value, nil
}

func randomHexString(size int) (string, error) {
	if size <= 0 {
		return "", errors.New("size must be positive")
	}
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func maskToken(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 8 {
		return strings.Repeat("*", len(trimmed))
	}
	prefix := trimmed[:4]
	suffix := trimmed[len(trimmed)-4:]
	middleMaskLen := len(trimmed) - 8
	if middleMaskLen < 6 {
		middleMaskLen = 6
	}
	return prefix + strings.Repeat("*", middleMaskLen) + suffix
}

func boolToInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func contextBackground() context.Context {
	return context.Background()
}
