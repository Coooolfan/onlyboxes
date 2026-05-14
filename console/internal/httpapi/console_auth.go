package httpapi

import (
	"errors"
	"sync"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

const (
	dashboardSessionCookieName      = "onlyboxes_console_session"
	dashboardSessionMaxAgeSec       = 12 * 60 * 60
	dashboardUsernamePrefix         = "admin-"
	dashboardUsernameRandomByteSize = 4
	dashboardPasswordRandomByteSize = 24
	dashboardPasswordHashAlgo       = "bcrypt"
	dashboardPasswordBCryptCost     = 12
	accountIDPrefix                 = "acc_"
	accountIDRandomByteSize         = 16
	maxAccountUsernameRunes         = 64
	initialAdminAPIKeyName          = "initial-admin"

	requestAccountIDGinKey       = "request_account_id"
	requestAccountUsernameGinKey = "request_account_username"
	requestAccountIsAdminGinKey  = "request_account_is_admin"
	requestAuthMethodGinKey       = "request_auth_method"
	requestAuthMethodCookie       = "cookie"
	requestAuthMethodAPIKey       = "api_key"
	requestAuthMethodDashboardJIT = "dashboard_jit"
)

var (
	dashboardSessionTTL               = 12 * time.Hour
	errAccountUsernameRequired        = errors.New("username is required")
	errAccountUsernameTooLong         = errors.New("username length must be <= 64")
	errAccountPasswordRequired        = errors.New("password is required")
	errAccountCurrentPasswordRequired = errors.New("current_password is required")
	errAccountNewPasswordRequired     = errors.New("new_password is required")
	errAccountCurrentPasswordInvalid  = errors.New("invalid current password")
	errAccountRegistrationDisabled    = errors.New("registration is disabled")
	errAccountRegistrationConflict    = errors.New("username already exists")
	errAccountInvalidCredentialRecord = errors.New("invalid account credential record")
	errAccountNotFound                = errors.New("account not found")
	errAccountDeleteSelfForbidden     = errors.New("cannot delete current account")
	errAccountDeleteAdminForbidden    = errors.New("cannot delete admin account")
	ErrConsoleQueriesRequired         = errors.New("console auth requires non-nil queries")
	accountIDGenerator                = generateAccountID
)

type DashboardCredentials struct {
	Username string
	Password string
}

type AdminAccountInitResult struct {
	AccountID         string
	Username          string
	PasswordPlaintext string
	InitializedNow    bool
	EnvIgnored        bool
	APIKeyInitialized bool
	APIKeyName        string
	APIKeyPlaintext   string
}

type SessionAccount struct {
	AccountID string `json:"account_id"`
	Username  string `json:"username"`
	IsAdmin   bool   `json:"is_admin"`
}

type accountSessionState struct {
	Account   SessionAccount
	ExpiresAt time.Time
}

type accountContextKey struct{}

type consoleAccountContext struct {
	AccountID string
	Username  string
	IsAdmin   bool
}

type ConsoleAuth struct {
	queries             *sqlc.Queries
	db                  *persistence.DB
	dashboardJITVerifier *jitTokenVerifier
	registrationEnabled bool

	sessionMu sync.Mutex
	sessions  map[string]accountSessionState
	nowFn     func() time.Time
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type registerAccountRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type accountSessionResponse struct {
	Authenticated       bool           `json:"authenticated,omitempty"`
	Account             SessionAccount `json:"account"`
	RegistrationEnabled bool           `json:"registration_enabled"`
	ConsoleVersion      string         `json:"console_version"`
	ConsoleRepoURL      string         `json:"console_repo_url"`
}

type registerAccountResponse struct {
	Account   SessionAccount `json:"account"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type accountListItem struct {
	AccountID string    `json:"account_id"`
	Username  string    `json:"username"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type accountListResponse struct {
	Items    []accountListItem `json:"items"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

type createAccountInput struct {
	Username string
	Password string
	IsAdmin  bool
	Now      time.Time
}

type createdAccount struct {
	AccountID string
	Username  string
	IsAdmin   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type accountInsertConflict int

const (
	accountInsertConflictUnknown accountInsertConflict = iota
	accountInsertConflictAccountID
	accountInsertConflictUsernameKey
)

func NewConsoleAuth(queries *sqlc.Queries, registrationEnabled bool) (*ConsoleAuth, error) {
	if queries == nil {
		return nil, ErrConsoleQueriesRequired
	}
	return &ConsoleAuth{
		queries:             queries,
		registrationEnabled: registrationEnabled,
		sessions:            make(map[string]accountSessionState),
		nowFn:               time.Now,
	}, nil
}

// SetPersistenceDB wires a *persistence.DB into ConsoleAuth so it can ensure
// JIT-derived accounts when authenticating dashboard JIT tokens.
func (a *ConsoleAuth) SetPersistenceDB(db *persistence.DB) {
	if a == nil {
		return
	}
	a.db = db
}

// SetDashboardJITSigningKey enables dashboard JIT authentication.
// The key MUST differ from the MCP JIT signing key — the two channels share
// the (iss, sub) → account derivation, but each owns its own signing material
// so that a leak of one key never lets an attacker forge tokens on the other.
// Pass empty to disable.
func (a *ConsoleAuth) SetDashboardJITSigningKey(key string) {
	if a == nil {
		return
	}
	verifier := newDashboardJITTokenVerifier(key)
	if verifier != nil {
		verifier.nowFn = a.nowFn
	}
	a.dashboardJITVerifier = verifier
}
