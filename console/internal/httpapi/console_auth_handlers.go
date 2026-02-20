package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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

func (a *ConsoleAuth) Logout(c *gin.Context) {
	if sessionID, err := c.Cookie(dashboardSessionCookieName); err == nil {
		a.deleteSession(sessionID)
	}

	a.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

func (a *ConsoleAuth) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
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
		c.Next()
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
