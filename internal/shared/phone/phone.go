// Package phone represents an Indian mobile number as a validated value object.
//
// It lives in the shared kernel beside money and id, rather than inside identity,
// because more than one bounded context legitimately holds a phone number: a user
// account has one, and so does a vendor we are trying to sign up who does not have
// an account yet and may never get one. Modules cannot import each other, so the
// alternative to sharing this is a second copy of the parser.
//
// A second copy is the thing to avoid, and not for tidiness. The country-code
// stripping below is length-aware because the obvious version is WRONG in a way
// that is invisible in testing: unconditionally removing a leading "91" corrupts
// "9123456780" -- a perfectly valid mobile that merely begins with 91 -- into an
// eight-digit number, silently locking out a real slice of customers. A copy made
// in a hurry gets that wrong again.
package phone

import (
	"regexp"
	"strings"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

// Phone is a mobile number in E.164 form (+91XXXXXXXXXX).
//
// A distinct type rather than a string, so an unvalidated number cannot reach the
// database: the only way to obtain one is through New, which normalises and
// validates. That also removes a whole class of duplicate-record bug where
// "9876543210", "+919876543210" and "09876543210" are three different rows.
type Phone string

// indianMobile matches a 10-digit Indian mobile number. The leading digit must be
// 6-9; 0-5 are landline or invalid ranges.
var indianMobile = regexp.MustCompile(`^[6-9]\d{9}$`)

// New normalises a human-entered number to E.164.
func New(raw string) (Phone, error) {
	// Strip everything a human might type: spaces, dashes, brackets, dots.
	cleaned := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		if r == '+' {
			return r
		}
		return -1
	}, raw)

	// Length-aware on purpose -- see the package comment. Only remove a prefix when
	// what remains is exactly the ten digits of a national number.
	switch {
	case strings.HasPrefix(cleaned, "+91") && len(cleaned) == 13:
		cleaned = cleaned[3:]
	case strings.HasPrefix(cleaned, "91") && len(cleaned) == 12:
		cleaned = cleaned[2:]
	case strings.HasPrefix(cleaned, "0") && len(cleaned) == 11:
		cleaned = cleaned[1:]
	}

	if !indianMobile.MatchString(cleaned) {
		return "", apperr.Invalid("INVALID_PHONE", "Enter a valid 10-digit Indian mobile number.").
			WithField("phone", "invalid_format")
	}
	return Phone("+91" + cleaned), nil
}

func (p Phone) String() string { return string(p) }

// National renders the number without the country code, for display.
func (p Phone) National() string { return strings.TrimPrefix(string(p), "+91") }

// Masked renders the number for support UIs and logs, e.g. "+91XXXXXX3210".
// Logging full phone numbers is a privacy liability.
func (p Phone) Masked() string {
	s := string(p)
	if len(s) < 6 {
		return "XXXXXX"
	}
	return s[:3] + "XXXXXX" + s[len(s)-4:]
}
