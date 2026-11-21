package outbox

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// Projects run CreateTableV1 from migrations that are already applied, so its
// SQL must never change. Add a MigrateV2 instead of editing it.
func TestSchemaV1IsFrozen(t *testing.T) {
	const want = "f2058abdafc11d69da3847ae2d19d54dbb38676bf72fbc90e273da30a0da0ba0"
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(schemaV1, "\n"))))
	if got != want {
		t.Fatalf("schemaV1 changed (checksum %s); add a new versioned migration function instead", got)
	}
}
