package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
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
	ID        string
	Name      string
	NameKey   string
	Token     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type MCPAuth struct {
	mu          sync.RWMutex
	recordsByID map[string]trustedTokenRecord
	tokenToID   map[string]string
	nameKeyToID map[string]string
	orderedIDs  []string
	nowFn       func() time.Time
}

func NewMCPAuth() *MCPAuth {
	return &MCPAuth{
		recordsByID: make(map[string]trustedTokenRecord),
		tokenToID:   make(map[string]string),
		nameKeyToID: make(map[string]string),
		orderedIDs:  []string{},
		nowFn:       time.Now,
	}
}

func (a *MCPAuth) RequireToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimSpace(c.GetHeader(trustedTokenHeader))
		if token == "" || a == nil || !a.isAllowed(token) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
			c.Abort()
			return
		}
		setRequestOwnerID(c, ownerIDFromToken(token))
		c.Next()
	}
}

func (a *MCPAuth) ListTokens(c *gin.Context) {
	items := []trustedTokenItem{}
	if a == nil {
		c.JSON(http.StatusOK, trustedTokenListResponse{Items: items, Total: 0})
		return
	}

	a.mu.RLock()
	for _, id := range a.orderedIDs {
		record, ok := a.recordsByID[id]
		if !ok {
			continue
		}
		items = append(items, trustedTokenItem{
			ID:          record.ID,
			Name:        record.Name,
			TokenMasked: maskToken(record.Token),
			CreatedAt:   record.CreatedAt,
			UpdatedAt:   record.UpdatedAt,
		})
	}
	a.mu.RUnlock()

	c.JSON(http.StatusOK, trustedTokenListResponse{
		Items: items,
		Total: len(items),
	})
}

func (a *MCPAuth) isAllowed(token string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	_, ok := a.tokenToID[token]
	a.mu.RUnlock()
	return ok
}

func (a *MCPAuth) CreateToken(c *gin.Context) {
	if a == nil {
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
		TokenMasked: maskToken(record.Token),
		Generated:   generated,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
	})
}

func (a *MCPAuth) DeleteToken(c *gin.Context) {
	if a == nil {
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
	if a == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "token store is unavailable"})
		return
	}

	tokenID := strings.TrimSpace(c.Param("token_id"))
	if tokenID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token_id is required"})
		return
	}

	record, err := a.getToken(tokenID)
	if err != nil {
		if errors.Is(err, errTrustedTokenNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "token not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get token"})
		return
	}

	c.JSON(http.StatusOK, trustedTokenValueResponse{
		ID:    record.ID,
		Name:  record.Name,
		Token: record.Token,
	})
}

func (a *MCPAuth) createToken(name string, tokenInput *string) (trustedTokenRecord, bool, error) {
	normalizedName, nameKey, err := normalizeTokenName(name)
	if err != nil {
		return trustedTokenRecord{}, false, err
	}

	generated := tokenInput == nil

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.nameKeyToID[nameKey]; exists {
		return trustedTokenRecord{}, false, errTrustedTokenNameConflict
	}

	tokenValue := ""
	if tokenInput != nil {
		tokenValue, err = normalizeTokenValue(*tokenInput)
		if err != nil {
			return trustedTokenRecord{}, false, err
		}
		if _, exists := a.tokenToID[tokenValue]; exists {
			return trustedTokenRecord{}, false, errTrustedTokenValueConflict
		}
	} else {
		generatedToken, generateErr := a.nextGeneratedTokenLocked()
		if generateErr != nil {
			return trustedTokenRecord{}, false, generateErr
		}
		tokenValue = generatedToken
	}

	tokenID, err := a.nextTokenIDLocked()
	if err != nil {
		return trustedTokenRecord{}, false, err
	}

	now := time.Now()
	if a.nowFn != nil {
		now = a.nowFn()
	}

	record := trustedTokenRecord{
		ID:        tokenID,
		Name:      normalizedName,
		NameKey:   nameKey,
		Token:     tokenValue,
		CreatedAt: now,
		UpdatedAt: now,
	}
	a.recordsByID[tokenID] = record
	a.tokenToID[tokenValue] = tokenID
	a.nameKeyToID[nameKey] = tokenID
	a.orderedIDs = append(a.orderedIDs, tokenID)

	return record, generated, nil
}

func (a *MCPAuth) nextGeneratedTokenLocked() (string, error) {
	for i := 0; i < 8; i++ {
		generated, err := generateTrustedToken()
		if err != nil {
			return "", errTrustedTokenGenerateFailed
		}
		if _, exists := a.tokenToID[generated]; exists {
			continue
		}
		return generated, nil
	}
	return "", errTrustedTokenGenerateFailed
}

func (a *MCPAuth) nextTokenIDLocked() (string, error) {
	for i := 0; i < 8; i++ {
		id, err := generateTokenID()
		if err != nil {
			return "", errTrustedTokenIDGenerateFailed
		}
		if _, exists := a.recordsByID[id]; exists {
			continue
		}
		return id, nil
	}
	return "", errTrustedTokenIDGenerateFailed
}

func (a *MCPAuth) getToken(tokenID string) (trustedTokenRecord, error) {
	a.mu.RLock()
	record, ok := a.recordsByID[tokenID]
	a.mu.RUnlock()
	if !ok {
		return trustedTokenRecord{}, errTrustedTokenNotFound
	}
	return record, nil
}

func (a *MCPAuth) deleteToken(tokenID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	record, ok := a.recordsByID[tokenID]
	if !ok {
		return errTrustedTokenNotFound
	}

	delete(a.recordsByID, tokenID)
	delete(a.tokenToID, record.Token)
	delete(a.nameKeyToID, record.NameKey)
	for i, id := range a.orderedIDs {
		if id != tokenID {
			continue
		}
		a.orderedIDs = append(a.orderedIDs[:i], a.orderedIDs[i+1:]...)
		break
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

func maskToken(value string) string {
	trimmed := strings.TrimSpace(value)
	length := len(trimmed)
	if length == 0 {
		return ""
	}
	if length < 8 {
		return strings.Repeat("*", length)
	}
	return trimmed[:4] + strings.Repeat("*", length-8) + trimmed[length-4:]
}
