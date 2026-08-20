// Package token issues and verifies JWT access tokens and opaque refresh
// tokens.
//
// The split is deliberate:
//
//   - Access tokens are short-lived JWTs. They are stateless, so authorizing a
//     request needs no database round trip -- which is what lets the API scale
//     horizontally without the auth check becoming the bottleneck.
//   - Refresh tokens are long-lived opaque random strings stored hashed in
//     Postgres. They must be revocable (logout, stolen device, password change),
//     and a stateless token cannot be revoked. Only their SHA-256 is stored, so
//     a database leak does not hand an attacker usable sessions.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

const (
	issuer          = "chowk"
	audienceAPI     = "chowk-api"
	refreshTokenLen = 32 // bytes of entropy
)

// Claims is our JWT payload. Roles and kitchen scope are embedded so
// authorization is a pure function of the token.
type Claims struct {
	jwt.RegisteredClaims
	UserID    string   `json:"uid"`
	Roles     []string `json:"roles"`
	KitchenID string   `json:"kid,omitempty"`
	SessionID string   `json:"sid"`
}

type Manager struct {
	secret          []byte
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
}

func NewManager(cfg config.AuthConfig) *Manager {
	return &Manager{
		secret:          []byte(cfg.JWTSecret),
		accessTokenTTL:  cfg.AccessTokenTTL,
		refreshTokenTTL: cfg.RefreshTokenTTL,
	}
}

func (m *Manager) AccessTokenTTL() time.Duration  { return m.accessTokenTTL }
func (m *Manager) RefreshTokenTTL() time.Duration { return m.refreshTokenTTL }

// IssueAccessToken mints a signed JWT for a principal.
func (m *Manager) IssueAccessToken(p httpx.Principal, now time.Time) (string, time.Time, error) {
	expiresAt := now.Add(m.accessTokenTTL)

	roles := make([]string, len(p.Roles))
	for i, r := range p.Roles {
		roles[i] = string(r)
	}

	var kitchenID string
	if p.KitchenID != id.Nil {
		kitchenID = p.KitchenID.String()
	}

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   p.UserID.String(),
			Audience:  jwt.ClaimStrings{audienceAPI},
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			// NotBefore guards against clock skew between issuer and verifier.
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ID:        id.New().String(),
		},
		UserID:    p.UserID.String(),
		Roles:     roles,
		KitchenID: kitchenID,
		SessionID: p.SessionID.String(),
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// VerifyAccessToken parses and validates a token, returning the principal.
//
// The algorithm is pinned to HS256. Without pinning, an attacker can present a
// token with alg=none or alg=RS256 and bypass verification entirely -- the
// classic JWT confusion attack.
func (m *Manager) VerifyAccessToken(raw string) (httpx.Principal, error) {
	claims := &Claims{}

	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
		}
		return m.secret, nil
	},
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audienceAPI),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		// Distinguish expiry from tampering: the client should silently refresh
		// on the former and be logged out on the latter.
		if errors.Is(err, jwt.ErrTokenExpired) {
			return httpx.Principal{}, apperr.Unauthorized("TOKEN_EXPIRED", "Your session has expired. Please sign in again.").WithCause(err)
		}
		return httpx.Principal{}, apperr.Unauthorized("INVALID_TOKEN", "Your session is not valid. Please sign in again.").WithCause(err)
	}

	userID, err := id.Parse(claims.UserID)
	if err != nil {
		return httpx.Principal{}, apperr.Unauthorized("INVALID_TOKEN", "Your session is not valid. Please sign in again.").WithCause(err)
	}

	p := httpx.Principal{UserID: userID}

	if claims.SessionID != "" {
		if sid, err := id.Parse(claims.SessionID); err == nil {
			p.SessionID = sid
		}
	}
	if claims.KitchenID != "" {
		if kid, err := id.Parse(claims.KitchenID); err == nil {
			p.KitchenID = kid
		}
	}
	for _, r := range claims.Roles {
		p.Roles = append(p.Roles, httpx.Role(r))
	}
	return p, nil
}

// RefreshToken is a freshly minted refresh token: the plaintext goes to the
// client exactly once, the hash goes to the database.
type RefreshToken struct {
	Plaintext string
	Hash      string
	ExpiresAt time.Time
}

// NewRefreshToken generates a cryptographically random refresh token.
func (m *Manager) NewRefreshToken(now time.Time) (RefreshToken, error) {
	buf := make([]byte, refreshTokenLen)
	if _, err := rand.Read(buf); err != nil {
		return RefreshToken{}, fmt.Errorf("token: read entropy: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(buf)

	return RefreshToken{
		Plaintext: plaintext,
		Hash:      HashRefreshToken(plaintext),
		ExpiresAt: now.Add(m.refreshTokenTTL),
	}, nil
}

// HashRefreshToken hashes a refresh token for storage and lookup.
//
// Plain SHA-256 is correct here, not bcrypt: the token already has 256 bits of
// entropy, so there is nothing to brute-force, and a fast hash lets us look the
// token up by an indexed equality match instead of scanning every row.
func HashRefreshToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
