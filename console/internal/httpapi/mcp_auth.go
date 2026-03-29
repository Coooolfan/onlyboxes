package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
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
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	trustedTokenHeader = "Authorization"
	bearerTokenScheme  = "Bearer"
)

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
	ErrMCPPersistenceDBRequired     = errors.New("mcp auth requires non-nil persistence db")
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
	AccountID   string
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
	db          *persistence.DB
	queries     *sqlc.Queries
	hasher      *persistence.Hasher
	jitVerifier *jitTokenVerifier
	nowFn       func() time.Time
}

func NewMCPAuthWithPersistence(db *persistence.DB) (*MCPAuth, error) {
	if db == nil {
		return nil, ErrMCPPersistenceDBRequired
	}
	auth := &MCPAuth{
		db:      db,
		queries: db.Queries,
		hasher:  db.Hasher,
		nowFn:   time.Now,
	}
	return auth, nil
}

func (a *MCPAuth) RequireToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := parseBearerToken(c.GetHeader(trustedTokenHeader))
		if !ok || a == nil || a.hasher == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
			c.Abort()
			return
		}

		if isJITToken(token) {
			identity, ok := a.verifyAndEnsureJITAccount(c.Request.Context(), token)
			if !ok {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
				c.Abort()
				return
			}
			setRequestOwnerID(c, identity.AccountID)
			c.Next()
			return
		}

		record, ok := a.lookupTrustedToken(c.Request.Context(), token)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
			c.Abort()
			return
		}
		setRequestOwnerID(c, strings.TrimSpace(record.AccountID))
		c.Next()
	}
}

func parseBearerToken(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}
	parts := strings.Fields(trimmed)
	if len(parts) != 2 {
		return "", false
	}
	if !strings.EqualFold(parts[0], bearerTokenScheme) {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}

func (a *MCPAuth) ListTokens(c *gin.Context) {
	if a == nil || a.queries == nil {
		c.JSON(http.StatusOK, trustedTokenListResponse{Items: []trustedTokenItem{}, Total: 0})
		return
	}
	account, ok := requireSessionAccount(c)
	if !ok {
		return
	}

	records, err := a.queries.ListTrustedTokensByAccount(c.Request.Context(), account.AccountID)
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
	_, ok := a.lookupTrustedToken(context.Background(), token)
	return ok
}

func (a *MCPAuth) CreateToken(c *gin.Context) {
	if a == nil || a.queries == nil || a.hasher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "token store is unavailable"})
		return
	}
	account, ok := requireSessionAccount(c)
	if !ok {
		return
	}

	req := createTrustedTokenRequest{}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	record, generated, err := a.createToken(c.Request.Context(), account.AccountID, req.Name, req.Token)
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
	account, ok := requireSessionAccount(c)
	if !ok {
		return
	}

	tokenID := strings.TrimSpace(c.Param("token_id"))
	if tokenID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token_id is required"})
		return
	}

	err := a.deleteToken(c.Request.Context(), tokenID, account.AccountID)
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

func (a *MCPAuth) createToken(ctx context.Context, accountID string, name string, tokenInput *string) (trustedTokenRecord, bool, error) {
	normalizedAccountID := strings.TrimSpace(accountID)
	if normalizedAccountID == "" {
		return trustedTokenRecord{}, false, errors.New("account_id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
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
			AccountID:   normalizedAccountID,
			Name:        normalizedName,
			NameKey:     nameKey,
			Token:       tokenValue,
			TokenHash:   tokenHash,
			TokenMasked: maskToken(tokenValue),
			Generated:   generated,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		err = a.queries.InsertTrustedToken(ctx, sqlc.InsertTrustedTokenParams{
			TokenID:         record.ID,
			AccountID:       record.AccountID,
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

		if isSQLiteConstraintError(err) {
			conflict, classifyErr := a.classifyTrustedTokenInsertConflict(
				ctx,
				record.AccountID,
				record.NameKey,
				record.TokenHash,
				record.ID,
			)
			if classifyErr != nil {
				return trustedTokenRecord{}, false, classifyErr
			}
			switch conflict {
			case trustedTokenInsertConflictName:
				return trustedTokenRecord{}, false, errTrustedTokenNameConflict
			case trustedTokenInsertConflictTokenHash:
				if generated {
					continue
				}
				return trustedTokenRecord{}, false, errTrustedTokenValueConflict
			case trustedTokenInsertConflictTokenID:
				continue
			default:
				return trustedTokenRecord{}, false, err
			}
		}
		return trustedTokenRecord{}, false, err
	}

	if generated {
		return trustedTokenRecord{}, false, errTrustedTokenGenerateFailed
	}
	return trustedTokenRecord{}, false, errTrustedTokenValueConflict
}

func (a *MCPAuth) deleteToken(ctx context.Context, tokenID string, accountID string) error {
	if a == nil || a.queries == nil {
		return errors.New("token store is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := a.queries.DeleteTrustedTokenByIDAndAccount(ctx, sqlc.DeleteTrustedTokenByIDAndAccountParams{
		TokenID:   tokenID,
		AccountID: strings.TrimSpace(accountID),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return errTrustedTokenNotFound
	}
	return nil
}

func (a *MCPAuth) lookupTrustedToken(ctx context.Context, token string) (sqlc.TrustedToken, bool) {
	if a == nil || a.queries == nil || a.hasher == nil {
		return sqlc.TrustedToken{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tokenHash := a.hasher.Hash(strings.TrimSpace(token))
	record, err := a.queries.GetTrustedTokenByHash(ctx, tokenHash)
	if err != nil {
		return sqlc.TrustedToken{}, false
	}
	if strings.TrimSpace(record.AccountID) == "" {
		return sqlc.TrustedToken{}, false
	}
	return record, true
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

type trustedTokenInsertConflict int

const (
	trustedTokenInsertConflictUnknown trustedTokenInsertConflict = iota
	trustedTokenInsertConflictName
	trustedTokenInsertConflictTokenHash
	trustedTokenInsertConflictTokenID
)

func isSQLiteConstraintError(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT, sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return true
	default:
		return false
	}
}

func (a *MCPAuth) classifyTrustedTokenInsertConflict(
	ctx context.Context,
	accountID string,
	nameKey string,
	tokenHash string,
	tokenID string,
) (trustedTokenInsertConflict, error) {
	if a == nil || a.queries == nil {
		return trustedTokenInsertConflictUnknown, nil
	}

	_, err := a.queries.GetTrustedTokenByAccountAndNameKey(ctx, sqlc.GetTrustedTokenByAccountAndNameKeyParams{
		AccountID: accountID,
		NameKey:   nameKey,
	})
	if err == nil {
		return trustedTokenInsertConflictName, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return trustedTokenInsertConflictUnknown, err
	}

	_, err = a.queries.GetTrustedTokenByHash(ctx, tokenHash)
	if err == nil {
		return trustedTokenInsertConflictTokenHash, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return trustedTokenInsertConflictUnknown, err
	}

	_, err = a.queries.GetTrustedTokenByID(ctx, tokenID)
	if err == nil {
		return trustedTokenInsertConflictTokenID, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return trustedTokenInsertConflictUnknown, err
	}

	return trustedTokenInsertConflictUnknown, nil
}
