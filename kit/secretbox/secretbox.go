// Package secretbox encrypts small secrets stored in the database, such as
// third-party API keys or queued message bodies, with AES-256-GCM.
//
// Every value is bound to a context string (usually the row's ID) as
// authenticated data, so a sealed value copied to another row fails to open.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

const version = "v1:"

// ErrCannotOpen means the value is corrupt, was sealed with another key, or
// belongs to another context. The cause is not distinguished on purpose.
var ErrCannotOpen = errors.New("secretbox: value cannot be decrypted")

// ParseKey decodes a 32-byte key given in standard or URL base64, padded or
// not, as produced by `openssl rand -base64 32`. name is the environment
// variable, used in the error message.
func ParseKey(name, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(raw); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, errors.New(name + " must be 32 random bytes in base64 (openssl rand -base64 32)")
}

// Box seals and opens values with one key. It is safe for concurrent use.
type Box struct{ aead cipher.AEAD }

// New takes a 32-byte key, normally from ParseKey.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secretbox: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plain for context and returns printable text ("v1:" plus
// base64 of nonce and ciphertext) for a TEXT column.
func (b *Box) Seal(context string, plain []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return version + base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, plain, []byte(context))), nil
}

// Open decrypts a value from Seal with the same context.
func (b *Box) Open(context, sealed string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(sealed, version)
	if !ok {
		return nil, ErrCannotOpen
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return nil, ErrCannotOpen
	}
	plain, err := b.aead.Open(nil, raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():], []byte(context))
	if err != nil {
		return nil, ErrCannotOpen
	}
	return plain, nil
}
