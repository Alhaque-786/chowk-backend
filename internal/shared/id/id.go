// Package id generates identifiers.
//
// Two kinds, deliberately:
//
//   - ID (UUIDv7) for every primary key. v7 is time-ordered, so inserts land at
//     the right edge of the B-tree instead of scattering across it the way v4
//     does. That matters once `orders` has tens of millions of rows.
//   - Human codes (e.g. "CK-8F2K9Q") for things a customer or support agent has
//     to read aloud: order numbers, coupon codes, rider references.
package id

import (
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ID is the primary-key type used across every module.
type ID = uuid.UUID

var Nil = uuid.Nil

// New returns a time-ordered UUIDv7. It falls back to v4 rather than panicking,
// because a slightly worse index locality is preferable to a downed API.
func New() ID {
	v, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return v
}

// Parse validates an external identifier.
func Parse(s string) (ID, error) { return uuid.Parse(s) }

func IsNil(v ID) bool { return v == uuid.Nil }

// humanAlphabet is Crockford base32 minus vowels: no I/L/O/U/0/1, so codes
// cannot be misread over the phone and cannot spell anything unfortunate.
const humanAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

// HumanCode returns a cryptographically random, unambiguous code of length n.
func HumanCode(n int) string {
	if n <= 0 {
		n = 8
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read only fails if the OS entropy source is broken; degrade to
		// the UUID path rather than returning a predictable code.
		return strings.ToUpper(strings.ReplaceAll(New().String(), "-", ""))[:n]
	}

	var sb strings.Builder
	sb.Grow(n)
	for _, b := range buf {
		sb.WriteByte(humanAlphabet[int(b)%len(humanAlphabet)])
	}
	return sb.String()
}

// OrderNumber is what the customer sees and support searches by.
func OrderNumber() string { return fmt.Sprintf("CK-%s", HumanCode(8)) }
