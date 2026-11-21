package credential

import (
	"errors"
	"strings"
	"testing"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected format: %s", hash)
	}
	if !VerifyPassword(hash, "correct horse battery") || VerifyPassword(hash, "correct horse batterY") {
		t.Fatal("verification mismatch")
	}
	other, _ := HashPassword("correct horse battery")
	if other == hash {
		t.Fatal("salt was not random")
	}
}

func TestPasswordRejectsBadInputAndHashes(t *testing.T) {
	for _, p := range []string{"short", strings.Repeat("x", MaxPasswordBytes+1)} {
		if _, err := HashPassword(p); !errors.Is(err, apperror.ErrInvalid) {
			t.Fatalf("accepted password of %d bytes", len(p))
		}
	}
	hash, _ := HashPassword("correct horse battery")
	parts := strings.Split(hash, "$")
	for name, encoded := range map[string]string{
		"empty":        "",
		"bcrypt":       "$2a$10$abcdefghijklmnopqrstuu",
		"other params": strings.Replace(hash, "t=2", "t=1", 1),
		"bad salt":     strings.Join(append(append([]string{}, parts[:4]...), "!!", parts[5]), "$"),
		"short key":    strings.Join(append(append([]string{}, parts[:5]...), "AAAA"), "$"),
	} {
		if VerifyPassword(encoded, "correct horse battery") {
			t.Fatalf("%s hash verified", name)
		}
	}
	if VerifyPassword(hash, strings.Repeat("x", MaxPasswordBytes+1)) {
		t.Fatal("oversized password verified")
	}
}

func TestTokens(t *testing.T) {
	a, b := NewToken(), NewToken()
	if a == b || len(a) < 26 {
		t.Fatalf("weak tokens: %q %q", a, b)
	}
	if HashToken(a) != HashToken(a) || HashToken(a) == HashToken(b) || len(HashToken(a)) != 64 || HashToken(a) == a {
		t.Fatal("token hash is not a stable sha256 hex digest")
	}
}

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{" Alice@Example.COM ": "alice@example.com", "a.b+c@example.com": "a.b+c@example.com"} {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "no-at", "Alice <a@example.com>", "a@example.com, b@example.com", strings.Repeat("a", 250) + "@example.com"} {
		if _, err := NormalizeEmail(in); !errors.Is(err, apperror.ErrInvalid) {
			t.Fatalf("accepted %q", in)
		}
	}
}
