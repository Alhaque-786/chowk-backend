// Package paging provides cursor-based pagination.
//
// Offset pagination (LIMIT/OFFSET) is not offered on purpose. On a hot
// `orders` table, OFFSET 50000 makes Postgres walk and discard 50k rows, and
// concurrent inserts cause rows to be skipped or repeated across pages. Cursor
// pagination is O(log n) per page and stable under writes.
package paging

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

const (
	DefaultLimit = 20
	MaxLimit     = 100
)

type SortDirection string

const (
	Ascending  SortDirection = "asc"
	Descending SortDirection = "desc"
)

// Cursor encodes the position of the last row of the previous page. It carries
// both the sort key and the tie-breaking ID so the keyset predicate is total
// even when many rows share a timestamp.
type Cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        id.ID     `json:"i"`
}

// Encode renders a cursor as an opaque URL-safe token. Clients must treat it as
// a blob -- the encoding is ours to change.
func (c Cursor) Encode() string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor parses a client-supplied token.
func DecodeCursor(token string) (Cursor, error) {
	if token == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, apperr.Invalid("INVALID_CURSOR", "The pagination cursor is malformed.").WithCause(err)
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, apperr.Invalid("INVALID_CURSOR", "The pagination cursor is malformed.").WithCause(err)
	}
	return c, nil
}

func (c Cursor) IsZero() bool { return c.ID == id.Nil && c.CreatedAt.IsZero() }

// Request is the normalised pagination input a repository receives.
type Request struct {
	Cursor    Cursor
	Limit     int
	Direction SortDirection
}

// NewRequest clamps a client-supplied limit into a safe range so one caller
// cannot ask for a million rows.
func NewRequest(cursorToken string, limit int, dir SortDirection) (Request, error) {
	c, err := DecodeCursor(cursorToken)
	if err != nil {
		return Request{}, err
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if dir != Ascending && dir != Descending {
		dir = Descending
	}
	return Request{Cursor: c, Limit: limit, Direction: dir}, nil
}

// FetchLimit is the row count a repository should actually request: one extra
// row, used purely to detect whether another page exists without a COUNT query.
func (r Request) FetchLimit() int { return r.Limit + 1 }

// Page is a slice of results plus the token needed to continue.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// NewPage trims the sentinel row a repository fetched via FetchLimit and derives
// the next cursor from the last item actually returned.
func NewPage[T any](rows []T, req Request, cursorOf func(T) Cursor) Page[T] {
	hasMore := len(rows) > req.Limit
	if hasMore {
		rows = rows[:req.Limit]
	}

	page := Page[T]{Items: rows, HasMore: hasMore}
	if page.Items == nil {
		page.Items = []T{}
	}
	if hasMore && len(rows) > 0 {
		page.NextCursor = cursorOf(rows[len(rows)-1]).Encode()
	}
	return page
}

// SQL returns the keyset predicate and ORDER BY clause for this request,
// starting parameter placeholders at argOffset+1.
//
// It returns the fragment plus the args to append, so repositories build queries
// consistently instead of each hand-rolling a comparison that silently drops
// rows on timestamp ties.
func (r Request) SQL(sortColumn, idColumn string, argOffset int) (where string, orderBy string, args []any) {
	cmp, dir := "<", "DESC"
	if r.Direction == Ascending {
		cmp, dir = ">", "ASC"
	}

	orderBy = fmt.Sprintf("ORDER BY %s %s, %s %s", sortColumn, dir, idColumn, dir)

	if r.Cursor.IsZero() {
		return "", orderBy, nil
	}
	// Row-value comparison gives a total order across the composite key, so no
	// row is visited twice and none is skipped.
	where = fmt.Sprintf("(%s, %s) %s ($%d, $%d)", sortColumn, idColumn, cmp, argOffset+1, argOffset+2)
	return where, orderBy, []any{r.Cursor.CreatedAt, r.Cursor.ID}
}
