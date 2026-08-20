package server

import "github.com/zevoxa/cloud-kitchen-backend/internal/shared/apperr"

// notFoundErr is the response for an unrouted path. Returning our standard
// envelope rather than chi's bare text keeps the frontend's error handling
// uniform -- a typo'd URL should not be the one response shape it cannot parse.
func notFoundErr() error {
	return apperr.NotFound("ROUTE_NOT_FOUND", "That endpoint does not exist.")
}
