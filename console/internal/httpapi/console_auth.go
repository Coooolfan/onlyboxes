package httpapi

import (
	"errors"
	"sync"
	"time"

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

	requestAccountIDGinKey       = "request_account_id"
	requestAccountUsernameGinKey = "request_account_username"
	requestAccountIsAdminGinKey  = "request_account_is_admin"
)

var (
	dashboardSessionTTL               = 12 * time.Hour
	errAccountUsernameRequired        = errors.New("username is required")
	errAccountUsernameTooLong         = errors.New("username length must be <= 64")
	errAccountPasswordRequired        = errors.New("password is required")
	errAccountRegistrationDisabled    = errors.New("registration is disabled")
	errAccountRegistrationConflict    = errors.New("username already exists")
	errAccountInvalidCredentialRecord = errors.New("invalid account credential record")
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

type accountSessionResponse struct {
	Authenticated       bool           `json:"authenticated,omitempty"`
	Account             SessionAccount `json:"account"`
	RegistrationEnabled bool           `json:"registration_enabled"`
}

type registerAccountResponse struct {
	Account   SessionAccount `json:"account"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
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

func NewConsoleAuth(queries *sqlc.Queries, registrationEnabled bool) *ConsoleAuth {
	if queries == nil {
		panic("console auth requires non-nil queries")
	}
	return &ConsoleAuth{
		queries:             queries,
		registrationEnabled: registrationEnabled,
		sessions:            make(map[string]accountSessionState),
		nowFn:               time.Now,
	}
}
