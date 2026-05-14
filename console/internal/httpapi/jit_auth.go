package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

const (
	jitTokenPrefix            = "obx_jit_v1."
	dashboardJitTokenPrefix   = "obx_dashboard_jit_v1."
	dashboardJitScope         = "dashboard"
	jitAccountIDPrefix        = "acc_jit_"
	jitAccountIDIssuerMaxLen  = 20
	jitAccountIDSubjectMaxLen = 20
	jitAccountIDHashHexLen    = 12
	jitUsernamePrefix         = "jit_"
	jitUsernameHashHexLen     = 24
	jitAccountPasswordHash    = "jit-account-no-dashboard-login"
	jitAccountHashAlgo        = "jit-disabled"
)

type jitTokenClaims struct {
	Issuer          string `json:"iss"`
	Subject         string `json:"sub"`
	Scope           string `json:"scope,omitempty"`
	ExpiresAtUnixMs int64  `json:"exp,omitempty"`
}

type jitAccountIdentity struct {
	Issuer    string
	Subject   string
	AccountID string
	Username  string
}

type jitTokenVerifier struct {
	key           []byte
	tokenPrefix   string
	requiredScope string
	nowFn         func() time.Time
}

func newJITTokenVerifier(key string) *jitTokenVerifier {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return nil
	}
	return &jitTokenVerifier{
		key:         []byte(trimmed),
		tokenPrefix: jitTokenPrefix,
	}
}

func newDashboardJITTokenVerifier(key string) *jitTokenVerifier {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return nil
	}
	return &jitTokenVerifier{
		key:           []byte(trimmed),
		tokenPrefix:   dashboardJitTokenPrefix,
		requiredScope: dashboardJitScope,
	}
}

func isJITToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), jitTokenPrefix)
}

func isDashboardJITToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), dashboardJitTokenPrefix)
}

func (v *jitTokenVerifier) verify(token string) (jitAccountIdentity, bool) {
	if v == nil || len(v.key) == 0 {
		return jitAccountIdentity{}, false
	}
	trimmedToken := strings.TrimSpace(token)
	prefix := v.tokenPrefix
	if prefix == "" {
		prefix = jitTokenPrefix
	}
	if !strings.HasPrefix(trimmedToken, prefix) {
		return jitAccountIdentity{}, false
	}

	parts := strings.SplitN(strings.TrimPrefix(trimmedToken, prefix), ".", 2)
	if len(parts) != 2 {
		return jitAccountIdentity{}, false
	}
	payloadEncoded := strings.TrimSpace(parts[0])
	signatureEncoded := strings.TrimSpace(parts[1])
	if payloadEncoded == "" || signatureEncoded == "" {
		return jitAccountIdentity{}, false
	}

	signature, err := base64.RawURLEncoding.DecodeString(signatureEncoded)
	if err != nil {
		return jitAccountIdentity{}, false
	}
	mac := hmac.New(sha256.New, v.key)
	_, _ = mac.Write([]byte(prefix + payloadEncoded))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return jitAccountIdentity{}, false
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadEncoded)
	if err != nil {
		return jitAccountIdentity{}, false
	}
	claims := jitTokenClaims{}
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return jitAccountIdentity{}, false
	}

	if v.requiredScope != "" && strings.TrimSpace(claims.Scope) != v.requiredScope {
		return jitAccountIdentity{}, false
	}
	if claims.ExpiresAtUnixMs > 0 {
		now := time.Now()
		if v.nowFn != nil {
			now = v.nowFn()
		}
		if now.UnixMilli() >= claims.ExpiresAtUnixMs {
			return jitAccountIdentity{}, false
		}
	}

	return deriveJITAccountIdentity(claims)
}

func deriveJITAccountIdentity(claims jitTokenClaims) (jitAccountIdentity, bool) {
	rawIssuer := strings.TrimSpace(claims.Issuer)
	rawSubject := strings.TrimSpace(claims.Subject)
	if rawIssuer == "" || rawSubject == "" {
		return jitAccountIdentity{}, false
	}

	issuerPart := normalizeJITAccountIDPart(rawIssuer, jitAccountIDIssuerMaxLen)
	subjectPart := normalizeJITAccountIDPart(rawSubject, jitAccountIDSubjectMaxLen)
	if issuerPart == "" || subjectPart == "" {
		return jitAccountIdentity{}, false
	}

	digest := sha256.Sum256([]byte(rawIssuer + "\x00" + rawSubject))
	digestHex := hex.EncodeToString(digest[:])
	return jitAccountIdentity{
		Issuer:    rawIssuer,
		Subject:   rawSubject,
		AccountID: jitAccountIDPrefix + issuerPart + "_" + subjectPart + "_" + digestHex[:jitAccountIDHashHexLen],
		Username:  jitUsernamePrefix + digestHex[:jitUsernameHashHexLen],
	}, true
}

func normalizeJITAccountIDPart(value string, maxLen int) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" || maxLen <= 0 {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(normalized))
	lastDash := false
	for _, r := range normalized {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			builder.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}

	cleaned := strings.Trim(builder.String(), "-_")
	if cleaned == "" {
		return ""
	}
	if len(cleaned) > maxLen {
		cleaned = strings.Trim(cleaned[:maxLen], "-_")
	}
	return cleaned
}

func (a *MCPAuth) SetJITSigningKey(key string) {
	if a == nil {
		return
	}
	a.jitVerifier = newJITTokenVerifier(key)
}

func (a *MCPAuth) verifyAndEnsureJITAccount(ctx context.Context, token string) (jitAccountIdentity, bool) {
	if a == nil || a.db == nil || a.queries == nil {
		return jitAccountIdentity{}, false
	}
	identity, ok := a.jitVerifier.verify(token)
	if !ok {
		return jitAccountIdentity{}, false
	}
	// A valid JIT signature is sufficient to authenticate. We persist a
	// deterministic non-admin account so request ownership stays account-scoped.
	account, ok := ensureJITAccount(ctx, a.db, a.queries, a.nowFn, identity)
	if !ok {
		return jitAccountIdentity{}, false
	}
	identity.AccountID = strings.TrimSpace(account.AccountID)
	identity.Username = strings.TrimSpace(account.Username)
	return identity, true
}

func ensureJITAccount(ctx context.Context, db *persistence.DB, queries *sqlc.Queries, nowFn func() time.Time, identity jitAccountIdentity) (sqlc.Account, bool) {
	if db == nil || queries == nil {
		return sqlc.Account{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	accountID := strings.TrimSpace(identity.AccountID)
	username := strings.TrimSpace(identity.Username)
	if accountID == "" || username == "" {
		return sqlc.Account{}, false
	}

	account, err := queries.GetAccountByID(ctx, accountID)
	if err == nil {
		return account, true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return sqlc.Account{}, false
	}

	now := time.Now()
	if nowFn != nil {
		now = nowFn()
	}
	nowMS := now.UnixMilli()

	created := sqlc.Account{
		AccountID:       accountID,
		Username:        username,
		UsernameKey:     strings.ToLower(username),
		PasswordHash:    jitAccountPasswordHash,
		HashAlgo:        jitAccountHashAlgo,
		IsAdmin:         0,
		CreatedAtUnixMs: nowMS,
		UpdatedAtUnixMs: nowMS,
	}

	err = db.WithTx(ctx, func(q *sqlc.Queries) error {
		existing, getErr := q.GetAccountByID(ctx, accountID)
		if getErr == nil {
			created = existing
			return nil
		}
		if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}

		insertErr := q.InsertAccount(ctx, sqlc.InsertAccountParams{
			AccountID:       accountID,
			Username:        username,
			UsernameKey:     strings.ToLower(username),
			PasswordHash:    jitAccountPasswordHash,
			HashAlgo:        jitAccountHashAlgo,
			IsAdmin:         0,
			CreatedAtUnixMs: nowMS,
			UpdatedAtUnixMs: nowMS,
		})
		if insertErr == nil {
			return nil
		}
		if !isSQLiteConstraintError(insertErr) {
			return insertErr
		}

		existing, getErr = q.GetAccountByID(ctx, accountID)
		if getErr == nil {
			created = existing
			return nil
		}
		if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}
		return insertErr
	})
	if err != nil {
		return sqlc.Account{}, false
	}
	return created, true
}
