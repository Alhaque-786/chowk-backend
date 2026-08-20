// Package password hashes and verifies passwords, and generates OTPs.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/apperr"
)

// Hasher wraps bcrypt with a configured cost.
//
// bcrypt over SHA-family hashing because it is deliberately slow and salts
// automatically; over Argon2id only because bcrypt has no tuning footguns and
// no cgo. Cost 12 is roughly 250ms on current hardware -- slow enough to make
// offline cracking expensive, fast enough for a login endpoint.
type Hasher struct {
	cost int
}

func NewHasher(cost int) *Hasher {
	// Clamp into bcrypt's supported range so a misconfigured cost cannot make
	// every login either insecure or a 30-second request.
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		cost = 12
	}
	return &Hasher{cost: cost}
}

// Hash returns the bcrypt hash of a password.
//
// bcrypt silently truncates at 72 bytes, which would make two different long
// passwords interchangeable, so over-long input is rejected instead.
func (h *Hasher) Hash(plaintext string) (string, error) {
	if len(plaintext) > 72 {
		return "", apperr.Invalid("PASSWORD_TOO_LONG", "Password must be at most 72 characters.")
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(plaintext), h.cost)
	if err != nil {
		return "", fmt.Errorf("password: hash: %w", err)
	}
	return string(hashed), nil
}

// Verify checks a password against a stored hash. It returns a bool rather than
// an error for the mismatch case, since a wrong password is expected traffic,
// not an exceptional condition.
func (h *Hasher) Verify(hashed, plaintext string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plaintext))
	if err == nil {
		return true, nil
	}
	// Only a genuine mismatch is "wrong password". Anything else -- a truncated
	// hash, an unknown prefix, a future bcrypt version -- means the stored hash
	// is corrupt or from another scheme. That is a real bug worth surfacing, and
	// reporting it as a failed login would hide data corruption behind a
	// plausible-looking 401.
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	return false, fmt.Errorf("password: malformed stored hash: %w", err)
}

// NeedsRehash reports whether a stored hash was made with a weaker cost than
// currently configured, so it can be transparently upgraded on next login.
func (h *Hasher) NeedsRehash(hashed string) bool {
	cost, err := bcrypt.Cost([]byte(hashed))
	if err != nil {
		return true
	}
	return cost < h.cost
}

// DummyHash is compared against when a login is attempted for a phone or email
// that does not exist.
//
// Without this, "user not found" returns in microseconds while "wrong password"
// takes 250ms, and that timing difference lets an attacker enumerate which
// phone numbers are registered. Burning the same CPU either way closes it.
const DummyHash = "$2a$12$LQv3c1yqBWVHxkd0LHAkCOYz6TtxMQJqhN8/LewdBPj4J/HS.7i5e"

// ValidateStrength enforces a password policy.
//
// Length is weighted over character-class rules because it contributes far more
// entropy in practice; the class requirement only kicks in for shorter
// passwords, so a long passphrase is not rejected for lacking a symbol.
func ValidateStrength(plaintext string) error {
	const minLen = 8

	if len(plaintext) < minLen {
		return apperr.Invalid("PASSWORD_TOO_SHORT",
			fmt.Sprintf("Password must be at least %d characters.", minLen)).
			WithField("password", "too_short")
	}
	if len(plaintext) > 72 {
		return apperr.Invalid("PASSWORD_TOO_LONG", "Password must be at most 72 characters.").
			WithField("password", "too_long")
	}

	var hasUpper, hasLower, hasDigit, hasSymbol bool
	for _, r := range plaintext {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			hasSymbol = true
		}
	}

	// A 16+ character passphrase clears the bar on length alone.
	if len(plaintext) < 16 {
		classes := 0
		for _, has := range []bool{hasUpper, hasLower, hasDigit, hasSymbol} {
			if has {
				classes++
			}
		}
		if classes < 3 {
			return apperr.Invalid("PASSWORD_TOO_WEAK",
				"Password must mix at least three of: uppercase, lowercase, digits, symbols -- or be 16+ characters.").
				WithField("password", "too_weak")
		}
	}

	if isCommon(plaintext) {
		return apperr.Invalid("PASSWORD_TOO_COMMON",
			"This password is too common. Please choose something less predictable.").
			WithField("password", "too_common")
	}
	return nil
}

// commonPasswords is a minimal denylist covering what shows up most in Indian
// credential dumps. Swap for a full HaveIBeenPwned k-anonymity check before
// launch -- this is a floor, not a solution.
var commonPasswords = map[string]struct{}{
	"password": {}, "password1": {}, "password123": {}, "12345678": {},
	"123456789": {}, "1234567890": {}, "qwerty123": {}, "iloveyou": {},
	"admin123": {}, "welcome1": {}, "abc123456": {}, "india123": {},
	"pass@123": {}, "admin@123": {}, "welcome@123": {},
}

func isCommon(plaintext string) bool {
	_, found := commonPasswords[strings.ToLower(plaintext)]
	return found
}

// GenerateOTP returns a cryptographically random numeric code.
//
// crypto/rand, not math/rand: a predictable OTP is equivalent to no OTP, and
// math/rand seeded by time is guessable within a window.
func GenerateOTP(digits int) (string, error) {
	if digits < 4 || digits > 10 {
		digits = 6
	}

	var sb strings.Builder
	sb.Grow(digits)
	for i := 0; i < digits; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", fmt.Errorf("password: generate otp: %w", err)
		}
		sb.WriteString(n.String())
	}
	return sb.String(), nil
}

// CompareOTP checks a submitted OTP in constant time, so response latency
// cannot reveal how many leading digits were correct.
func CompareOTP(expected, submitted string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(submitted)) == 1
}
