package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
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

// TestLoadTokens covers the multi-token env parsing behind token-as-session
// isolation: the comma list form (valid multi, whitespace trimming, order
// preserved), the single-var fallback, the list-wins precedence when both
// variables are set, and the rejections — empty entry, invalid entry,
// duplicate — which must identify the offending entry by 1-based position
// and must NEVER echo the value (tokens are credentials; this error text
// goes to stderr). Duplicates are rejected rather than silently deduped so
// a typo'd list cannot silently drop an endpoint.
func TestLoadTokens(t *testing.T) {
	t.Run("list form, order preserved", func(t *testing.T) {
		t.Setenv(tokensEnv, "tok-alpha,tok-beta,tok-gamma")
		t.Setenv(tokenEnv, "")
		got, err := loadTokens()
		if err != nil {
			t.Fatalf("loadTokens: %v", err)
		}
		want := []string{"tok-alpha", "tok-beta", "tok-gamma"}
		if !slices.Equal(got, want) {
			t.Errorf("loadTokens = %v, want %v", got, want)
		}
	})

	t.Run("whitespace around commas trimmed", func(t *testing.T) {
		t.Setenv(tokensEnv, " tok-alpha ,  tok-beta\t,\ttok-gamma ")
		t.Setenv(tokenEnv, "")
		got, err := loadTokens()
		if err != nil {
			t.Fatalf("loadTokens: %v", err)
		}
		want := []string{"tok-alpha", "tok-beta", "tok-gamma"}
		if !slices.Equal(got, want) {
			t.Errorf("loadTokens = %v, want %v (entries must be trimmed)", got, want)
		}
	})

	t.Run("single-var fallback when list unset", func(t *testing.T) {
		t.Setenv(tokensEnv, "")
		t.Setenv(tokenEnv, "solo-token")
		got, err := loadTokens()
		if err != nil {
			t.Fatalf("loadTokens: %v", err)
		}
		if !slices.Equal(got, []string{"solo-token"}) {
			t.Errorf("loadTokens = %v, want [solo-token] via the single-var fallback", got)
		}
	})

	t.Run("single-var fallback when list empty", func(t *testing.T) {
		t.Setenv(tokensEnv, "")
		t.Setenv(tokenEnv, "solo-token")
		if got, err := loadTokens(); err != nil || !slices.Equal(got, []string{"solo-token"}) {
			t.Errorf("loadTokens = %v (%v), want [solo-token] via the single-var fallback", got, err)
		}
	})

	t.Run("list wins when both set", func(t *testing.T) {
		t.Setenv(tokensEnv, "list-one,list-two")
		t.Setenv(tokenEnv, "ignored-solo-token-XYZ")
		got, err := loadTokens()
		if err != nil {
			t.Fatalf("loadTokens: %v", err)
		}
		want := []string{"list-one", "list-two"}
		if !slices.Equal(got, want) {
			t.Errorf("loadTokens = %v, want %v (TOKENS wins over TOKEN)", got, want)
		}
	})

	t.Run("required error when both unset", func(t *testing.T) {
		t.Setenv(tokensEnv, "")
		t.Setenv(tokenEnv, "")
		_, err := loadTokens()
		if err == nil {
			t.Fatal("loadTokens with both vars empty: want error, got nil")
		}
		if !strings.Contains(err.Error(), tokenEnv) || !strings.Contains(err.Error(), "token-path") {
			t.Errorf("fallback error must name %s and the token-path model: %v", tokenEnv, err)
		}
	})

	t.Run("empty entry rejected by position", func(t *testing.T) {
		t.Setenv(tokensEnv, "tok-alpha,,tok-beta")
		t.Setenv(tokenEnv, "")
		_, err := loadTokens()
		assertEntryError(t, err, 2, "")
	})

	t.Run("all-empty list rejected", func(t *testing.T) {
		t.Setenv(tokensEnv, ",")
		t.Setenv(tokenEnv, "")
		_, err := loadTokens()
		assertEntryError(t, err, 1, "")
	})

	t.Run("invalid entry rejected by position, value never echoed", func(t *testing.T) {
		const bad = "bad-slash-secret-token/x"
		t.Setenv(tokensEnv, "tok-alpha,"+bad)
		t.Setenv(tokenEnv, "")
		_, err := loadTokens()
		assertEntryError(t, err, 2, bad)
	})

	t.Run("duplicate rejected by position, value never echoed", func(t *testing.T) {
		const dup = "dup-secret-token-XYZ"
		t.Setenv(tokensEnv, dup+",tok-alpha,"+dup)
		t.Setenv(tokenEnv, "")
		_, err := loadTokens()
		assertEntryError(t, err, 3, dup)
	})
}

// assertEntryError checks one loadTokens rejection: err is non-nil, names
// tokensEnv, identifies the offending entry by its 1-based position, and —
// when secret is non-empty — does NOT echo the offending value (tokens are
// credentials; startup errors go to stderr).
func assertEntryError(t *testing.T, err error, pos int, secret string) {
	t.Helper()
	if err == nil {
		t.Fatalf("loadTokens: want an entry-%d error, got nil", pos)
	}
	msg := err.Error()
	if !strings.Contains(msg, tokensEnv) {
		t.Errorf("error must name %s: %s", tokensEnv, msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("entry %d", pos)) {
		t.Errorf("error must identify the offending entry by position %d: %s", pos, msg)
	}
	if secret != "" && strings.Contains(msg, secret) {
		t.Errorf("error echoes the offending token value %q: %s", secret, msg)
	}
}
