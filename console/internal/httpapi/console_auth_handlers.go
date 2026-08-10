package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

func (a *ConsoleAuth) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	account, ok := a.lookupAccount(c.Request.Context(), req.Username)
	if !ok || !a.verifyPassword(account, req.Password) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
		return
	}

	sessionAccount := SessionAccount{
		AccountID: strings.TrimSpace(account.AccountID),
		Username:  strings.TrimSpace(account.Username),
		IsAdmin:   account.IsAdmin == 1,
	}
	sessionID, expiresAt, err := a.createSession(sessionAccount)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}

	a.setSessionCookie(c, sessionID, expiresAt)
	c.JSON(http.StatusOK, accountSessionResponse{
		Authenticated:       true,
		Account:             sessionAccount,
		RegistrationEnabled: a.registrationEnabled,
		ConsoleVersion:      consoleVersion(),
		ConsoleRepoURL:      consoleRepoURL(),
	})
}

func (a *ConsoleAuth) Session(c *gin.Context) {
	account, ok := requireSessionAccount(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, accountSessionResponse{
		Authenticated:       true,
		Account:             account,
		RegistrationEnabled: a.registrationEnabled,
		ConsoleVersion:      consoleVersion(),
		ConsoleRepoURL:      consoleRepoURL(),
	})
}

func (a *ConsoleAuth) Register(c *gin.Context) {
	if !a.registrationEnabled {
		c.JSON(http.StatusForbidden, gin.H{"error": errAccountRegistrationDisabled.Error()})
		return
	}

	account, ok := requireSessionAccount(c)
	if !ok {
		return
	}
	if !account.IsAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin access required"})
		return
	}

	var req registerAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	now := time.Now()
	if a.nowFn != nil {
		now = a.nowFn()
	}

	created, err := createAccountWithRetry(c.Request.Context(), a.queries, createAccountInput{
		Username: req.Username,
		Password: req.Password,
		IsAdmin:  false,
		Now:      now,
	})
	if err != nil {
		switch {
		case errors.Is(err, errAccountUsernameRequired),
			errors.Is(err, errAccountUsernameTooLong),
			errors.Is(err, errAccountPasswordRequired):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, errAccountRegistrationConflict):
			c.JSON(http.StatusConflict, gin.H{"error": errAccountRegistrationConflict.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create account"})
		}
		return
	}

	c.JSON(http.StatusCreated, registerAccountResponse{
		Account: SessionAccount{
			AccountID: created.AccountID,
			Username:  created.Username,
			IsAdmin:   created.IsAdmin,
		},
		CreatedAt: created.CreatedAt,
		UpdatedAt: created.UpdatedAt,
	})
}

func (a *ConsoleAuth) ChangePassword(c *gin.Context) {
	sessionAccount, ok := requireSessionAccount(c)
	if !ok {
		return
	}

	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	currentPassword := strings.TrimSpace(req.CurrentPassword)
	if currentPassword == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errAccountCurrentPasswordRequired.Error()})
		return
	}
	newPassword := strings.TrimSpace(req.NewPassword)
	if newPassword == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errAccountNewPasswordRequired.Error()})
		return
	}

	accountRecord, err := a.queries.GetAccountByID(c.Request.Context(), strings.TrimSpace(sessionAccount.AccountID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.deleteSessionsByAccountID(sessionAccount.AccountID)
			a.clearSessionCookie(c)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update password"})
		return
	}
	if !a.verifyPassword(accountRecord, currentPassword) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errAccountCurrentPasswordInvalid.Error()})
		return
	}

	newPasswordHash, err := hashDashboardPassword(newPassword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update password"})
		return
	}

	now := time.Now()
	if a.nowFn != nil {
		now = a.nowFn()
	}

	updatedRows, err := a.queries.UpdateAccountPasswordByID(c.Request.Context(), sqlc.UpdateAccountPasswordByIDParams{
		PasswordHash:    newPasswordHash,
		HashAlgo:        dashboardPasswordHashAlgo,
		UpdatedAtUnixMs: now.UnixMilli(),
		AccountID:       strings.TrimSpace(sessionAccount.AccountID),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update password"})
		return
	}
	if updatedRows == 0 {
		a.deleteSessionsByAccountID(sessionAccount.AccountID)
		a.clearSessionCookie(c)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	renewedAccount := SessionAccount{
		AccountID: strings.TrimSpace(accountRecord.AccountID),
		Username:  strings.TrimSpace(accountRecord.Username),
		IsAdmin:   accountRecord.IsAdmin == 1,
	}
	a.deleteSessionsByAccountID(renewedAccount.AccountID)

	sessionID, expiresAt, err := a.createSession(renewedAccount)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to refresh session"})
		return
	}

	a.setSessionCookie(c, sessionID, expiresAt)
	c.Status(http.StatusNoContent)
}

func (a *ConsoleAuth) ListAccounts(c *gin.Context) {
	page, ok := parsePositiveIntQuery(c, "page", 1)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
		return
	}
	pageSize, ok := parsePositiveIntQuery(c, "page_size", 20)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be a positive integer"})
		return
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	total, err := a.queries.CountAccounts(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list accounts"})
		return
	}

	offset := (page - 1) * pageSize
	records, err := a.queries.ListAccountsPage(c.Request.Context(), sqlc.ListAccountsPageParams{
		Limit:  int64(pageSize),
		Offset: int64(offset),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list accounts"})
		return
	}

	items := make([]accountListItem, 0, len(records))
	for _, record := range records {
		items = append(items, accountListItem{
			AccountID: strings.TrimSpace(record.AccountID),
			Username:  strings.TrimSpace(record.Username),
			IsAdmin:   record.IsAdmin == 1,
			CreatedAt: time.UnixMilli(record.CreatedAtUnixMs),
			UpdatedAt: time.UnixMilli(record.UpdatedAtUnixMs),
		})
	}

	c.JSON(http.StatusOK, accountListResponse{
		Items:    items,
		Total:    int(total),
		Page:     page,
		PageSize: pageSize,
	})
}

func (a *ConsoleAuth) DeleteAccount(c *gin.Context) {
	currentAccount, ok := requireSessionAccount(c)
	if !ok {
		return
	}

	targetAccountID := strings.TrimSpace(c.Param("account_id"))
	if targetAccountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id is required"})
		return
	}
	if targetAccountID == strings.TrimSpace(currentAccount.AccountID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errAccountDeleteSelfForbidden.Error()})
		return
	}

	deletedRows, err := a.queries.DeleteNonAdminAccountByID(c.Request.Context(), targetAccountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete account"})
		return
	}
	if deletedRows == 0 {
		record, lookupErr := a.queries.GetAccountByID(c.Request.Context(), targetAccountID)
		if lookupErr != nil {
			if errors.Is(lookupErr, sql.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": errAccountNotFound.Error()})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete account"})
			return
		}
		if record.IsAdmin == 1 {
			c.JSON(http.StatusForbidden, gin.H{"error": errAccountDeleteAdminForbidden.Error()})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": errAccountNotFound.Error()})
		return
	}

	if a.proxyRouteRevoker != nil {
		a.proxyRouteRevoker.RevokeOwnerRoutes(targetAccountID)
	}
	a.deleteSessionsByAccountID(targetAccountID)
	c.Status(http.StatusNoContent)
}

func (a *ConsoleAuth) Logout(c *gin.Context) {
	if sessionID, err := c.Cookie(dashboardSessionCookieName); err == nil {
		a.deleteSession(sessionID)
	}

	a.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

func (a *ConsoleAuth) RequireAuth(apiKeyAuth *APIKeyAuth) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader(trustedTokenHeader)
		token, hasBearer := parseBearerToken(authHeader)
		if hasBearer {
			if isJITToken(token) {
				// MCP-channel JIT tokens are intentionally rejected here so they
				// cannot reach dashboard endpoints. Dashboard JIT tokens use a
				// separate prefix and are handled below.
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing api key"})
				c.Abort()
				return
			}
			if isDashboardJITToken(token) {
				if a.dashboardJITVerifier == nil || a.db == nil || a.queries == nil {
					c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing api key"})
					c.Abort()
					return
				}
				identity, ok := a.dashboardJITVerifier.verify(token)
				if !ok {
					c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing api key"})
					c.Abort()
					return
				}
				account, ok := ensureJITAccount(c.Request.Context(), a.db, a.queries, a.nowFn, identity)
				if !ok {
					c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing api key"})
					c.Abort()
					return
				}
				setRequestSessionAccount(c, SessionAccount{
					AccountID: strings.TrimSpace(account.AccountID),
					Username:  strings.TrimSpace(account.Username),
					IsAdmin:   false,
				})
				c.Set(requestAuthMethodGinKey, requestAuthMethodDashboardJIT)
				c.Next()
				return
			}
			if apiKeyAuth == nil {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing api key"})
				c.Abort()
				return
			}

			record, ok := apiKeyAuth.lookupAPIKey(c.Request.Context(), token)
			if !ok {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing api key"})
				c.Abort()
				return
			}

			accountRecord, err := a.queries.GetAccountByID(c.Request.Context(), strings.TrimSpace(record.AccountID))
			if err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing api key"})
				c.Abort()
				return
			}

			setRequestSessionAccount(c, SessionAccount{
				AccountID: strings.TrimSpace(accountRecord.AccountID),
				Username:  strings.TrimSpace(accountRecord.Username),
				IsAdmin:   accountRecord.IsAdmin == 1,
			})
			c.Set(requestAuthMethodGinKey, requestAuthMethodAPIKey)
			c.Next()
			return
		}

		sessionID, err := c.Cookie(dashboardSessionCookieName)
		if err != nil || strings.TrimSpace(sessionID) == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			c.Abort()
			return
		}

		sessionState, ok := a.sessionState(sessionID)
		if !ok {
			a.clearSessionCookie(c)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			c.Abort()
			return
		}

		setRequestSessionAccount(c, sessionState.Account)
		c.Set(requestAuthMethodGinKey, requestAuthMethodCookie)
		c.Next()
	}
}

func (a *ConsoleAuth) RequireCookieSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		methodValue, ok := c.Get(requestAuthMethodGinKey)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			c.Abort()
			return
		}

		method, ok := methodValue.(string)
		if !ok || strings.TrimSpace(method) == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			c.Abort()
			return
		}

		switch method {
		case requestAuthMethodCookie:
			c.Next()
		case requestAuthMethodAPIKey:
			c.JSON(http.StatusForbidden, gin.H{"error": "this endpoint requires cookie session authentication"})
			c.Abort()
		default:
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			c.Abort()
		}
	}
}

func (a *ConsoleAuth) RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		account, ok := requestSessionAccountFromGin(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			c.Abort()
			return
		}
		if !account.IsAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			c.Abort()
			return
		}
		c.Next()
	}
}
