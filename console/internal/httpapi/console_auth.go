package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
	"golang.org/x/crypto/bcrypt"
)

const (
	dashboardSessionCookieName      = "onlyboxes_console_session"
	dashboardSessionMaxAgeSec       = 12 * 60 * 60
	dashboardUsernamePrefix         = "admin-"
	dashboardUsernameRandomByteSize = 4
	dashboardPasswordRandomByteSize = 24
	dashboardPasswordHashAlgo       = "bcrypt"
	dashboardPasswordBCryptCost     = 12
	dashboardCredentialSingletonID  = 1
)

var dashboardSessionTTL = 12 * time.Hour

type DashboardCredentials struct {
	Username string
	Password string
}

type DashboardCredentialMaterial struct {
	Username     string
	PasswordHash string
	HashAlgo     string
}

type DashboardCredentialInitResult struct {
	Username          string
	PasswordHash      string
	HashAlgo          string
	PasswordPlaintext string
	InitializedNow    bool
	EnvIgnored        bool
}

type ConsoleAuth struct {
	username     string
	passwordHash string
	hashAlgo     string

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

func InitializeDashboardCredentials(
	ctx context.Context,
	queries *sqlc.Queries,
	envUsername string,
	envPassword string,
) (DashboardCredentialInitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if queries == nil {
		return DashboardCredentialInitResult{}, errors.New("dashboard credential queries is required")
	}

	record, err := queries.GetDashboardCredential(ctx)
	if err == nil {
		username := strings.TrimSpace(record.Username)
		passwordHash := strings.TrimSpace(record.PasswordHash)
		hashAlgo := strings.TrimSpace(record.HashAlgo)
		if username == "" || passwordHash == "" {
			return DashboardCredentialInitResult{}, errors.New("persisted dashboard credential is invalid")
		}
		if !strings.EqualFold(hashAlgo, dashboardPasswordHashAlgo) {
			return DashboardCredentialInitResult{}, fmt.Errorf(
				"persisted dashboard credential hash algorithm %q is unsupported",
				hashAlgo,
			)
		}
		return DashboardCredentialInitResult{
			Username:          username,
			PasswordHash:      passwordHash,
			HashAlgo:          dashboardPasswordHashAlgo,
			PasswordPlaintext: "",
			InitializedNow:    false,
			EnvIgnored:        envUsername != "" || envPassword != "",
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DashboardCredentialInitResult{}, fmt.Errorf("load persisted dashboard credential: %w", err)
	}

	credentials, err := ResolveDashboardCredentials(envUsername, envPassword)
	if err != nil {
		return DashboardCredentialInitResult{}, err
	}
	nowMS := time.Now().UnixMilli()
	passwordHash, err := hashDashboardPassword(credentials.Password)
	if err != nil {
		return DashboardCredentialInitResult{}, fmt.Errorf("hash dashboard password: %w", err)
	}
	if err := queries.InsertDashboardCredential(ctx, sqlc.InsertDashboardCredentialParams{
		SingletonID:     dashboardCredentialSingletonID,
		Username:        credentials.Username,
		PasswordHash:    passwordHash,
		HashAlgo:        dashboardPasswordHashAlgo,
		CreatedAtUnixMs: nowMS,
		UpdatedAtUnixMs: nowMS,
	}); err != nil {
		return DashboardCredentialInitResult{}, fmt.Errorf("persist dashboard credential: %w", err)
	}

	return DashboardCredentialInitResult{
		Username:          credentials.Username,
		PasswordHash:      passwordHash,
		HashAlgo:          dashboardPasswordHashAlgo,
		PasswordPlaintext: credentials.Password,
		InitializedNow:    true,
		EnvIgnored:        false,
	}, nil
}

func NewConsoleAuth(material DashboardCredentialMaterial) *ConsoleAuth {
	return &ConsoleAuth{
		username:     strings.TrimSpace(material.Username),
		passwordHash: strings.TrimSpace(material.PasswordHash),
		hashAlgo:     strings.TrimSpace(material.HashAlgo),
		sessions:     make(map[string]time.Time),
		nowFn:        time.Now,
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
	if a == nil {
		return false
	}
	if !strings.EqualFold(a.hashAlgo, dashboardPasswordHashAlgo) {
		return false
	}
	usernameOK := constantTimeEqualString(a.username, username)
	passwordOK := compareDashboardPassword(a.passwordHash, password)
	return usernameOK && passwordOK
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

func hashDashboardPassword(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), dashboardPasswordBCryptCost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

func compareDashboardPassword(hash string, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(strings.TrimSpace(hash)), []byte(password)) == nil
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
