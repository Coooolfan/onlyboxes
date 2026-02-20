package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
	"golang.org/x/crypto/bcrypt"
)

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

func InitializeAdminAccount(
	ctx context.Context,
	queries *sqlc.Queries,
	envUsername string,
	envPassword string,
) (AdminAccountInitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if queries == nil {
		return AdminAccountInitResult{}, errors.New("account queries is required")
	}

	adminCount, err := queries.CountAdminAccounts(ctx)
	if err != nil {
		return AdminAccountInitResult{}, fmt.Errorf("count admin accounts: %w", err)
	}
	if adminCount > 0 {
		adminAccount, err := queries.GetFirstAdminAccount(ctx)
		if err != nil {
			return AdminAccountInitResult{}, fmt.Errorf("load admin account: %w", err)
		}
		return AdminAccountInitResult{
			AccountID:      strings.TrimSpace(adminAccount.AccountID),
			Username:       strings.TrimSpace(adminAccount.Username),
			InitializedNow: false,
			EnvIgnored:     envUsername != "" || envPassword != "",
		}, nil
	}

	credentials, err := ResolveDashboardCredentials(envUsername, envPassword)
	if err != nil {
		return AdminAccountInitResult{}, err
	}

	created, err := createAccountWithRetry(ctx, queries, createAccountInput{
		Username: credentials.Username,
		Password: credentials.Password,
		IsAdmin:  true,
		Now:      time.Now(),
	})
	if err != nil {
		return AdminAccountInitResult{}, fmt.Errorf("insert admin account: %w", err)
	}

	return AdminAccountInitResult{
		AccountID:         created.AccountID,
		Username:          created.Username,
		PasswordPlaintext: credentials.Password,
		InitializedNow:    true,
		EnvIgnored:        false,
	}, nil
}

func (a *ConsoleAuth) lookupAccount(ctx context.Context, username string) (sqlc.Account, bool) {
	if a == nil || a.queries == nil {
		return sqlc.Account{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, usernameKey, err := normalizeAccountUsername(username)
	if err != nil {
		return sqlc.Account{}, false
	}
	account, err := a.queries.GetAccountByUsernameKey(ctx, usernameKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.Account{}, false
		}
		return sqlc.Account{}, false
	}
	if strings.TrimSpace(account.AccountID) == "" || strings.TrimSpace(account.PasswordHash) == "" {
		return sqlc.Account{}, false
	}
	return account, true
}

func (a *ConsoleAuth) verifyPassword(account sqlc.Account, password string) bool {
	if strings.TrimSpace(password) == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(account.HashAlgo), dashboardPasswordHashAlgo) {
		return false
	}
	return compareDashboardPassword(account.PasswordHash, password)
}

func normalizeAccountUsername(value string) (string, string, error) {
	username := strings.TrimSpace(value)
	if username == "" {
		return "", "", errAccountUsernameRequired
	}
	if utf8.RuneCountInString(username) > maxAccountUsernameRunes {
		return "", "", errAccountUsernameTooLong
	}
	return username, strings.ToLower(username), nil
}

func createAccountWithRetry(
	ctx context.Context,
	queries *sqlc.Queries,
	input createAccountInput,
) (createdAccount, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if queries == nil {
		return createdAccount{}, errors.New("account queries is required")
	}

	normalizedUsername, usernameKey, err := normalizeAccountUsername(input.Username)
	if err != nil {
		return createdAccount{}, err
	}
	if strings.TrimSpace(input.Password) == "" {
		return createdAccount{}, errAccountPasswordRequired
	}

	passwordHash, err := hashDashboardPassword(input.Password)
	if err != nil {
		return createdAccount{}, fmt.Errorf("hash account password: %w", err)
	}

	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}
	nowMS := now.UnixMilli()

	for i := 0; i < 8; i++ {
		accountID, err := accountIDGenerator()
		if err != nil {
			return createdAccount{}, fmt.Errorf("generate account id: %w", err)
		}
		err = queries.InsertAccount(ctx, sqlc.InsertAccountParams{
			AccountID:       accountID,
			Username:        normalizedUsername,
			UsernameKey:     usernameKey,
			PasswordHash:    passwordHash,
			HashAlgo:        dashboardPasswordHashAlgo,
			IsAdmin:         boolToInt64(input.IsAdmin),
			CreatedAtUnixMs: nowMS,
			UpdatedAtUnixMs: nowMS,
		})
		if err == nil {
			createdAt := time.UnixMilli(nowMS)
			return createdAccount{
				AccountID: accountID,
				Username:  normalizedUsername,
				IsAdmin:   input.IsAdmin,
				CreatedAt: createdAt,
				UpdatedAt: createdAt,
			}, nil
		}

		if isSQLiteConstraintError(err) {
			conflict, classifyErr := classifyAccountInsertConflict(ctx, queries, accountID, usernameKey)
			if classifyErr != nil {
				return createdAccount{}, classifyErr
			}
			switch conflict {
			case accountInsertConflictAccountID:
				continue
			case accountInsertConflictUsernameKey:
				return createdAccount{}, errAccountRegistrationConflict
			default:
				return createdAccount{}, err
			}
		}
		return createdAccount{}, err
	}

	return createdAccount{}, errors.New("failed to create account")
}

func classifyAccountInsertConflict(
	ctx context.Context,
	queries *sqlc.Queries,
	accountID string,
	usernameKey string,
) (accountInsertConflict, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if queries == nil {
		return accountInsertConflictUnknown, nil
	}

	_, err := queries.GetAccountByID(ctx, strings.TrimSpace(accountID))
	if err == nil {
		return accountInsertConflictAccountID, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return accountInsertConflictUnknown, err
	}

	_, err = queries.GetAccountByUsernameKey(ctx, strings.TrimSpace(usernameKey))
	if err == nil {
		return accountInsertConflictUsernameKey, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return accountInsertConflictUnknown, err
	}

	return accountInsertConflictUnknown, nil
}

func generateAccountID() (string, error) {
	randomPart, err := randomHex(accountIDRandomByteSize)
	if err != nil {
		return "", err
	}
	return accountIDPrefix + randomPart, nil
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
