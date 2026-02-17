package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	dashboardSessionCookieName      = "onlyboxes_console_session"
	dashboardSessionMaxAgeSec       = 12 * 60 * 60
	dashboardUsernamePrefix         = "admin-"
	dashboardUsernameRandomByteSize = 4
	dashboardPasswordRandomByteSize = 24
)

var dashboardSessionTTL = 12 * time.Hour

type DashboardCredentials struct {
	Username string
	Password string
}

type ConsoleAuth struct {
	credentials DashboardCredentials

	sessionMu sync.Mutex
	sessions  map[string]time.Time
	nowFn     func() time.Time
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func ResolveDashboardCredentials(username string, password string) (DashboardCredentials, error) {
	credentials := DashboardCredentials{
		Username: strings.TrimSpace(username),
		Password: password,
	}

	if credentials.Username == "" {
		suffix, err := randomHex(dashboardUsernameRandomByteSize)
		if err != nil {
			return DashboardCredentials{}, err
		}
		credentials.Username = dashboardUsernamePrefix + suffix
	}
	if credentials.Password == "" {
		secret, err := randomHex(dashboardPasswordRandomByteSize)
		if err != nil {
			return DashboardCredentials{}, err
		}
		credentials.Password = secret
	}

	return credentials, nil
}

func NewConsoleAuth(credentials DashboardCredentials) *ConsoleAuth {
	return &ConsoleAuth{
		credentials: credentials,
		sessions:    make(map[string]time.Time),
		nowFn:       time.Now,
	}
}

func (a *ConsoleAuth) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !a.checkCredentials(req.Username, req.Password) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
		return
	}

	sessionID, expiresAt, err := a.createSession()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}

	a.setSessionCookie(c, sessionID, expiresAt)
	c.JSON(http.StatusOK, gin.H{"authenticated": true})
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
		if !a.isSessionValid(sessionID) {
			a.clearSessionCookie(c)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			c.Abort()
			return
		}

		c.Next()
	}
}

func (a *ConsoleAuth) checkCredentials(username string, password string) bool {
	return constantTimeEqualString(a.credentials.Username, username) &&
		constantTimeEqualString(a.credentials.Password, password)
}

func (a *ConsoleAuth) createSession() (string, time.Time, error) {
	sessionID, err := randomHex(32)
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := a.nowFn().Add(dashboardSessionTTL)

	a.sessionMu.Lock()
	a.sessions[sessionID] = expiresAt
	a.sessionMu.Unlock()

	return sessionID, expiresAt, nil
}

func (a *ConsoleAuth) isSessionValid(sessionID string) bool {
	now := a.nowFn()

	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()

	expiresAt, ok := a.sessions[sessionID]
	if !ok {
		return false
	}
	if !expiresAt.After(now) {
		delete(a.sessions, sessionID)
		return false
	}
	return true
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

func constantTimeEqualString(expected string, actual string) bool {
	expectedHash := sha256.Sum256([]byte(expected))
	actualHash := sha256.Sum256([]byte(actual))
	return subtle.ConstantTimeCompare(expectedHash[:], actualHash[:]) == 1
}

func randomHex(byteSize int) (string, error) {
	if byteSize <= 0 {
		return "", errors.New("byteSize must be positive")
	}

	raw := make([]byte, byteSize)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
