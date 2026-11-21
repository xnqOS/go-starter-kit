package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newBox(t *testing.T) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseKeyAcceptsCommonBase64Forms(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		got, err := ParseKey("APP_SECRETS_KEY", " "+enc.EncodeToString(key)+"\n")
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("%v: %v", enc, err)
		}
	}
	for _, raw := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := ParseKey("APP_SECRETS_KEY", raw); err == nil || !strings.Contains(err.Error(), "APP_SECRETS_KEY") {
			t.Fatalf("accepted %q: %v", raw, err)
		}
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	b := newBox(t)
	sealed, err := b.Seal("row-1", []byte("sk_live_secret"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "v1:") || strings.Contains(sealed, "sk_live") {
		t.Fatalf("unexpected sealed form: %s", sealed)
	}
	again, _ := b.Seal("row-1", []byte("sk_live_secret"))
	if again == sealed {
		t.Fatal("nonce reused")
	}
	plain, err := b.Open("row-1", sealed)
	if err != nil || string(plain) != "sk_live_secret" {
		t.Fatalf("open: %q %v", plain, err)
	}
	empty, _ := b.Seal("row-1", nil)
	if plain, err := b.Open("row-1", empty); err != nil || len(plain) != 0 {
		t.Fatalf("empty value: %v", err)
	}
}

func TestOpenRejectsMovedTamperedOrForeignValues(t *testing.T) {
	b := newBox(t)
	sealed, _ := b.Seal("row-1", []byte("secret"))
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "v1:"))
	raw[len(raw)-1] ^= 1
	tampered := "v1:" + base64.StdEncoding.EncodeToString(raw)
	for name, open := range map[string]func() ([]byte, error){
		"other row":  func() ([]byte, error) { return b.Open("row-2", sealed) },
		"other key":  func() ([]byte, error) { return other.Open("row-1", sealed) },
		"tampered":   func() ([]byte, error) { return b.Open("row-1", tampered) },
		"no version": func() ([]byte, error) { return b.Open("row-1", strings.TrimPrefix(sealed, "v1:")) },
		"not base64": func() ([]byte, error) { return b.Open("row-1", "v1:!!!") },
		"too short":  func() ([]byte, error) { return b.Open("row-1", "v1:AAAA") },
	} {
		if _, err := open(); !errors.Is(err, ErrCannotOpen) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("short key accepted")
	}
}
