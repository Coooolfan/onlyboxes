package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (a *ConsoleAuth) createSession(account SessionAccount) (string, time.Time, error) {
	sessionID, err := randomHex(32)
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now()
	if a != nil && a.nowFn != nil {
		now = a.nowFn()
	}
	expiresAt := now.Add(dashboardSessionTTL)

	a.sessionMu.Lock()
	a.sessions[sessionID] = accountSessionState{
		Account:   account,
		ExpiresAt: expiresAt,
	}
	a.sessionMu.Unlock()

	return sessionID, expiresAt, nil
}

func (a *ConsoleAuth) sessionState(sessionID string) (accountSessionState, bool) {
	now := time.Now()
	if a != nil && a.nowFn != nil {
		now = a.nowFn()
	}

	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()

	state, ok := a.sessions[sessionID]
	if !ok {
		return accountSessionState{}, false
	}
	if !state.ExpiresAt.After(now) {
		delete(a.sessions, sessionID)
		return accountSessionState{}, false
	}
	return state, true
}

func (a *ConsoleAuth) deleteSession(sessionID string) {
	a.sessionMu.Lock()
	delete(a.sessions, sessionID)
	a.sessionMu.Unlock()
}

func (a *ConsoleAuth) setSessionCookie(c *gin.Context, sessionID string, expiresAt time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     dashboardSessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   dashboardSessionMaxAgeSec,
		Expires:  expiresAt,
		Secure:   requestIsTLS(c.Request),
	})
}

func (a *ConsoleAuth) clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     dashboardSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		Secure:   requestIsTLS(c.Request),
	})
}

func setRequestSessionAccount(c *gin.Context, account SessionAccount) {
	if c == nil {
		return
	}
	accountID := strings.TrimSpace(account.AccountID)
	username := strings.TrimSpace(account.Username)
	if accountID == "" || username == "" {
		return
	}

	c.Set(requestAccountIDGinKey, accountID)
	c.Set(requestAccountUsernameGinKey, username)
	c.Set(requestAccountIsAdminGinKey, account.IsAdmin)

	if c.Request != nil {
		ctxValue := consoleAccountContext{
			AccountID: accountID,
			Username:  username,
			IsAdmin:   account.IsAdmin,
		}
		ctx := context.WithValue(c.Request.Context(), accountContextKey{}, ctxValue)
		c.Request = c.Request.WithContext(ctx)
	}
}

func requestSessionAccountFromContext(ctx context.Context) (SessionAccount, bool) {
	if ctx == nil {
		return SessionAccount{}, false
	}
	value := ctx.Value(accountContextKey{})
	ctxValue, ok := value.(consoleAccountContext)
	if !ok {
		return SessionAccount{}, false
	}
	accountID := strings.TrimSpace(ctxValue.AccountID)
	username := strings.TrimSpace(ctxValue.Username)
	if accountID == "" || username == "" {
		return SessionAccount{}, false
	}
	return SessionAccount{
		AccountID: accountID,
		Username:  username,
		IsAdmin:   ctxValue.IsAdmin,
	}, true
}

func requestSessionAccountFromGin(c *gin.Context) (SessionAccount, bool) {
	if c == nil {
		return SessionAccount{}, false
	}

	accountIDValue, ok := c.Get(requestAccountIDGinKey)
	if ok {
		usernameValue, usernameOK := c.Get(requestAccountUsernameGinKey)
		isAdminValue, isAdminOK := c.Get(requestAccountIsAdminGinKey)
		accountID, accountIDOK := accountIDValue.(string)
		username, usernameTypeOK := usernameValue.(string)
		isAdmin, isAdminTypeOK := isAdminValue.(bool)
		if usernameOK && isAdminOK && accountIDOK && usernameTypeOK && isAdminTypeOK {
			account := SessionAccount{
				AccountID: strings.TrimSpace(accountID),
				Username:  strings.TrimSpace(username),
				IsAdmin:   isAdmin,
			}
			if account.AccountID != "" && account.Username != "" {
				return account, true
			}
		}
	}

	if c.Request != nil {
		return requestSessionAccountFromContext(c.Request.Context())
	}
	return SessionAccount{}, false
}

func requireSessionAccount(c *gin.Context) (SessionAccount, bool) {
	account, ok := requestSessionAccountFromGin(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		c.Abort()
		return SessionAccount{}, false
	}
	return account, true
}

func requestIsTLS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}

	forwardedProto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if forwardedProto == "" {
		return false
	}

	parts := strings.Split(forwardedProto, ",")
	if len(parts) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(parts[0]), "https")
}
