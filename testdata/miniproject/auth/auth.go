// Package auth handles authentication.
package auth

// Token is an opaque auth token.
type Token string

// Validate returns true for non-empty tokens.
func Validate(t Token) bool {
	return t != ""
}

// hello
