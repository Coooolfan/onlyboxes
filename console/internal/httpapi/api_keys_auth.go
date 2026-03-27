package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

const (
	apiKeyPrefix       = "obxk_"
	apiKeyIDPrefix     = "apik_"
	apiKeyByteLength   = 32
	apiKeyIDByteLength = 16
	maxAPIKeyNameRunes = 64
)

var (
	errAPIKeyNameRequired          = errors.New("name is required")
	errAPIKeyNameTooLong           = errors.New("name length must be <= 64")
	errAPIKeyNameConflict          = errors.New("api key name already exists")
	errAPIKeyNotFound              = errors.New("api key not found")
	errAPIKeyGenerateFailed        = errors.New("failed to generate api key")
	errAPIKeyIDGenerateFailed      = errors.New("failed to generate api key id")
	ErrAPIKeyPersistenceDBRequired = errors.New("api key auth requires non-nil persistence db")
)

type apiKeyItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	KeyMasked string    `json:"key_masked"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type apiKeyListResponse struct {
	Items []apiKeyItem `json:"items"`
	Total int          `json:"total"`
}

type createAPIKeyRequest struct {
	Name string `json:"name"`
}

type createAPIKeyResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	KeyMasked string    `json:"key_masked"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type apiKeyRecord struct {
	ID        string
	AccountID string
	Name      string
	NameKey   string
	Key       string
	KeyHash   string
	KeyMasked string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type APIKeyAuth struct {
	queries *sqlc.Queries
	hasher  *persistence.Hasher
	nowFn   func() time.Time
}

func NewAPIKeyAuth(db *persistence.DB) (*APIKeyAuth, error) {
	if db == nil {
		return nil, ErrAPIKeyPersistenceDBRequired
	}
	return &APIKeyAuth{
		queries: db.Queries,
		hasher:  db.Hasher,
		nowFn:   time.Now,
	}, nil
}

func (a *APIKeyAuth) ListAPIKeys(c *gin.Context) {
	if a == nil || a.queries == nil {
		c.JSON(http.StatusOK, apiKeyListResponse{Items: []apiKeyItem{}, Total: 0})
		return
	}
	account, ok := requireSessionAccount(c)
	if !ok {
		return
	}

	records, err := a.queries.ListAPIKeysByAccount(c.Request.Context(), account.AccountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list api keys"})
		return
	}

	items := make([]apiKeyItem, 0, len(records))
	for _, record := range records {
		items = append(items, apiKeyItem{
			ID:        record.ApiKeyID,
			Name:      record.Name,
			KeyMasked: record.KeyMasked,
			CreatedAt: time.UnixMilli(record.CreatedAtUnixMs),
			UpdatedAt: time.UnixMilli(record.UpdatedAtUnixMs),
		})
	}

	c.JSON(http.StatusOK, apiKeyListResponse{
		Items: items,
		Total: len(items),
	})
}

func (a *APIKeyAuth) CreateAPIKey(c *gin.Context) {
	if a == nil || a.queries == nil || a.hasher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api key store is unavailable"})
		return
	}
	account, ok := requireSessionAccount(c)
	if !ok {
		return
	}

	req := createAPIKeyRequest{}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	record, err := a.createAPIKey(c.Request.Context(), account.AccountID, req.Name, "")
	if err != nil {
		switch {
		case errors.Is(err, errAPIKeyNameRequired), errors.Is(err, errAPIKeyNameTooLong):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, errAPIKeyNameConflict):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create api key"})
		}
		return
	}

	c.JSON(http.StatusCreated, createAPIKeyResponse{
		ID:        record.ID,
		Name:      record.Name,
		Key:       record.Key,
		KeyMasked: record.KeyMasked,
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
	})
}

func (a *APIKeyAuth) DeleteAPIKey(c *gin.Context) {
	if a == nil || a.queries == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api key store is unavailable"})
		return
	}
	account, ok := requireSessionAccount(c)
	if !ok {
		return
	}

	apiKeyID := strings.TrimSpace(c.Param("api_key_id"))
	if apiKeyID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key_id is required"})
		return
	}

	err := a.deleteAPIKey(c.Request.Context(), apiKeyID, account.AccountID)
	if err != nil {
		if errors.Is(err, errAPIKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete api key"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *APIKeyAuth) lookupAPIKey(ctx context.Context, token string) (sqlc.ApiKey, bool) {
	if a == nil || a.queries == nil || a.hasher == nil {
		return sqlc.ApiKey{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	keyHash := a.hasher.Hash(strings.TrimSpace(token))
	record, err := a.queries.GetAPIKeyByHash(ctx, keyHash)
	if err != nil {
		return sqlc.ApiKey{}, false
	}
	if strings.TrimSpace(record.AccountID) == "" {
		return sqlc.ApiKey{}, false
	}
	return record, true
}

func (a *APIKeyAuth) createAPIKey(ctx context.Context, accountID string, name string, keyOverride string) (apiKeyRecord, error) {
	normalizedAccountID := strings.TrimSpace(accountID)
	if normalizedAccountID == "" {
		return apiKeyRecord{}, errors.New("account_id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	normalizedName, nameKey, err := normalizeAPIKeyName(name)
	if err != nil {
		return apiKeyRecord{}, err
	}

	for i := 0; i < 8; i++ {
		var keyValue string
		if keyOverride != "" {
			keyValue = keyOverride
		} else {
			var genErr error
			keyValue, genErr = generateAPIKey()
			if genErr != nil {
				return apiKeyRecord{}, errAPIKeyGenerateFailed
			}
		}

		keyID, idErr := generateAPIKeyID()
		if idErr != nil {
			return apiKeyRecord{}, errAPIKeyIDGenerateFailed
		}

		now := time.Now()
		if a.nowFn != nil {
			now = a.nowFn()
		}

		record := apiKeyRecord{
			ID:        keyID,
			AccountID: normalizedAccountID,
			Name:      normalizedName,
			NameKey:   nameKey,
			Key:       keyValue,
			KeyHash:   a.hasher.Hash(keyValue),
			KeyMasked: maskToken(keyValue),
			CreatedAt: now,
			UpdatedAt: now,
		}

		err = a.queries.InsertAPIKey(ctx, sqlc.InsertAPIKeyParams{
			ApiKeyID:        record.ID,
			AccountID:       record.AccountID,
			Name:            record.Name,
			NameKey:         record.NameKey,
			KeyHash:         record.KeyHash,
			KeyMasked:       record.KeyMasked,
			CreatedAtUnixMs: record.CreatedAt.UnixMilli(),
			UpdatedAtUnixMs: record.UpdatedAt.UnixMilli(),
		})
		if err == nil {
			return record, nil
		}

		if isSQLiteConstraintError(err) {
			conflict, classifyErr := a.classifyAPIKeyInsertConflict(
				ctx,
				record.AccountID,
				record.NameKey,
				record.KeyHash,
				record.ID,
			)
			if classifyErr != nil {
				return apiKeyRecord{}, classifyErr
			}
			switch conflict {
			case apiKeyInsertConflictName:
				return apiKeyRecord{}, errAPIKeyNameConflict
			case apiKeyInsertConflictKeyHash, apiKeyInsertConflictKeyID:
				continue
			default:
				return apiKeyRecord{}, err
			}
		}
		return apiKeyRecord{}, err
	}

	return apiKeyRecord{}, errAPIKeyGenerateFailed
}

func (a *APIKeyAuth) deleteAPIKey(ctx context.Context, apiKeyID string, accountID string) error {
	if a == nil || a.queries == nil {
		return errors.New("api key store is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := a.queries.DeleteAPIKeyByIDAndAccount(ctx, sqlc.DeleteAPIKeyByIDAndAccountParams{
		ApiKeyID:  apiKeyID,
		AccountID: strings.TrimSpace(accountID),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return errAPIKeyNotFound
	}
	return nil
}

func normalizeAPIKeyName(value string) (string, string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", "", errAPIKeyNameRequired
	}
	if utf8.RuneCountInString(trimmed) > maxAPIKeyNameRunes {
		return "", "", errAPIKeyNameTooLong
	}
	return trimmed, strings.ToLower(trimmed), nil
}

func generateAPIKey() (string, error) {
	value, err := randomHexString(apiKeyByteLength)
	if err != nil {
		return "", err
	}
	return apiKeyPrefix + value, nil
}

func generateAPIKeyID() (string, error) {
	value, err := randomHexString(apiKeyIDByteLength)
	if err != nil {
		return "", err
	}
	return apiKeyIDPrefix + value, nil
}

type apiKeyInsertConflict int

const (
	apiKeyInsertConflictUnknown apiKeyInsertConflict = iota
	apiKeyInsertConflictName
	apiKeyInsertConflictKeyHash
	apiKeyInsertConflictKeyID
)

func (a *APIKeyAuth) classifyAPIKeyInsertConflict(
	ctx context.Context,
	accountID string,
	nameKey string,
	keyHash string,
	keyID string,
) (apiKeyInsertConflict, error) {
	if a == nil || a.queries == nil {
		return apiKeyInsertConflictUnknown, nil
	}

	_, err := a.queries.GetAPIKeyByAccountAndNameKey(ctx, sqlc.GetAPIKeyByAccountAndNameKeyParams{
		AccountID: accountID,
		NameKey:   nameKey,
	})
	if err == nil {
		return apiKeyInsertConflictName, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return apiKeyInsertConflictUnknown, err
	}

	_, err = a.queries.GetAPIKeyByHash(ctx, keyHash)
	if err == nil {
		return apiKeyInsertConflictKeyHash, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return apiKeyInsertConflictUnknown, err
	}

	_, err = a.queries.GetAPIKeyByID(ctx, keyID)
	if err == nil {
		return apiKeyInsertConflictKeyID, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return apiKeyInsertConflictUnknown, err
	}

	return apiKeyInsertConflictUnknown, nil
}
