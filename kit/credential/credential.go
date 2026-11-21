// Package credential hashes passwords and opaque tokens and normalizes
// account emails. It has no HTTP or database dependencies.
//
// Password hashing is deliberately slow and uses about 19 MiB of memory per
// call; bound concurrent calls (see package admission) so a login burst
// cannot exhaust CPU or memory.
package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/mail"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 1024
	maxEmailBytes    = 254
)

// Argon2id parameters: the OWASP minimum recommendation (19 MiB, 2 passes).
// Changing them makes VerifyPassword reject existing hashes, so add support
// for the old parameters before raising them.
const (
	argonTime    = 2
	argonMemory  = 19456
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
	argonParams  = "m=19456,t=2,p=1"
)

// ValidatePassword checks length only; composition rules are left to the
// product. The upper bound keeps hashing cost predictable.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return apperror.Invalid("password must contain 12 to 1024 bytes")
	}
	return nil
}

// HashPassword returns a PHC-format argon2id hash
// ($argon2id$v=19$m=...,t=...,p=...$salt$key).
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return "$argon2id$v=19$" + argonParams + "$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

// VerifyPassword reports whether password matches encoded. Malformed hashes
// and hashes with other parameters never match.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != argonParams || len(password) > MaxPasswordBytes {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltLen {
		return false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != argonKeyLen {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(key, actual) == 1
}

// NewToken returns a random opaque token (128 bits, base32), suitable for
// session, reset and API tokens. Store only HashToken of it.
func NewToken() string { return rand.Text() }

// HashToken is the value to store and look up for a high-entropy token.
// A fast hash is enough because tokens are random, unlike passwords.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NormalizeEmail trims and lowercases an address and accepts only a bare
// address ("a@example.com", not "Name <a@example.com>").
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || len(email) > maxEmailBytes {
		return "", apperror.Invalid("invalid email")
	}
	return email, nil
}
