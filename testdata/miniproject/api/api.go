// Package api exposes the service endpoints.
package api

import "miniproject/auth"

// Handle processes a request with the given token.
func Handle(t auth.Token) string {
	if auth.Validate(t) {
		return "ok"
	}
	return "unauthorized"
}
