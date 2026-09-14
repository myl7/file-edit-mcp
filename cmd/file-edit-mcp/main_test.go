package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestIsNormalClose covers the predicate behind the exit-code decision:
// client-initiated session ends (stdin EOF, the SDK's internal
// "server is closing" sentinel text) exit 0; real failures stay errors.
func TestIsNormalClose(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil (clean Run return)", nil, false},
		{"bare io.EOF", io.EOF, true},
		{"wrapped io.EOF", fmt.Errorf("read frame: %w", io.EOF), true},
		{"joined with io.EOF", errors.Join(context.Canceled, io.EOF), true},
		{"server is closing: EOF (observed shape)", errors.New("server is closing: EOF"), true},
		{"bare server is closing", errors.New("server is closing"), true},
		{"generic failure", errors.New("listen: no such file"), false},
		{"context cancelled (SIGINT path)", context.Canceled, false},
		{"errConnClosed lookalike", errors.New("connection closed: server is not closing"), false},
	}
	for _, tc := range cases {
		if got := isNormalClose(tc.err); got != tc.want {
			t.Errorf("isNormalClose(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestValidateToken pins the token charset contract behind token-path auth:
// URL-path-segment safe, 1–128 chars, leading alphanumeric. The rejects are
// exactly the characters that would let a token alter the ServeMux route
// ("/", "{", "}") plus the shape rules; the accepts are both length
// boundaries and every allowed character class.
func TestValidateToken(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"empty", "", false},
		{"slash (ServeMux path separator)", "a/b", false},
		{"open brace (ServeMux wildcard)", "a{b}", false},
		{"close brace", "a}b", false},
		{"asterisk", "a*b", false},
		{"leading hyphen", "-abc", false},
		{"leading underscore", "_abc", false},
		{"leading dot", ".abc", false},
		{"space", "ab c", false},
		{"percent-escape attempt", "a%2Fb", false},
		{"129 chars (over the cap)", strings.Repeat("a", 129), false},
		{"128 chars with hyphens/underscores", "a" + strings.Repeat("-_", 63) + "z", true},
		{"single alphanumeric", "a", true},
		{"single digit", "7", true},
		{"uppercase mix", "Ab9", true},
		{"typical generated token", "k3X-whisky_Victor-42", true},
	}
	for _, tc := range cases {
		err := validateToken(tc.token)
		if (err == nil) != tc.want {
			t.Errorf("validateToken(%q) error = %v, want error = %v", tc.token, err, !tc.want)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), tokenEnv) {
			t.Errorf("validateToken(%q) error does not name %s: %v", tc.token, tokenEnv, err)
		}
	}
}

// TestLoadToken covers the run() startup gate for the http transport:
// missing/empty is the "required, enables token-path auth" error; a valid
// token round-trips; an invalid one is the charset error.
func TestLoadToken(t *testing.T) {
	t.Setenv(tokenEnv, "")
	if _, err := loadToken(); err == nil {
		t.Fatal("loadToken with empty env: want error, got nil")
	} else if !strings.Contains(err.Error(), tokenEnv) || !strings.Contains(err.Error(), "token-path") {
		t.Errorf("empty-env error must name %s and the token-path model: %v", tokenEnv, err)
	}

	t.Setenv(tokenEnv, "round-trip-token")
	got, err := loadToken()
	if err != nil {
		t.Fatalf("loadToken with valid token: %v", err)
	}
	if got != "round-trip-token" {
		t.Errorf("loadToken = %q, want the env value verbatim", got)
	}

	t.Setenv(tokenEnv, "bad/token")
	if _, err := loadToken(); err == nil {
		t.Fatal("loadToken with invalid token: want error, got nil")
	}
}
