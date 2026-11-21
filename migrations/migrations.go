// Package migrations contains versioned, frozen schema snapshots shared by both databases.
package migrations

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"

	"gorm.io/gorm"

	"github.com/xnqOS/go-starter-kit/kit/database"
)

// Embed each Go source to detect edits to already-applied migrations.
// The legacy SQL is archived only to recognize the original runner's checksum.
//
//go:embed [0-9]*.go legacy/*.up.sql
var sources embed.FS

// List returns explicitly registered migrations with checksums of their sources.
func List() ([]database.Migration, error) {
	definitions := []struct {
		id string
		up func(*gorm.DB) error
	}{
		{"000001_create_example", createExampleV000001},
		// scaffold:migrations
	}
	files, err := fs.Glob(sources, "[0-9]*.go")
	if err != nil {
		return nil, err
	}
	if len(files) != len(definitions) {
		return nil, fmt.Errorf("each versioned Go migration must be registered exactly once in migrations.go")
	}
	result := make([]database.Migration, 0, len(definitions))
	seen := make(map[string]bool)
	for _, definition := range definitions {
		if seen[definition.id] {
			return nil, fmt.Errorf("duplicate migration registration: %s", definition.id)
		}
		seen[definition.id] = true
		body, err := sources.ReadFile(definition.id + ".go")
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		migration := database.Migration{ID: definition.id, Checksum: fmt.Sprintf("%x", sum), Up: definition.up}
		if definition.id == "000001_create_example" {
			legacy, err := sources.ReadFile("legacy/000001_create_example.up.sql")
			if err != nil {
				return nil, err
			}
			checksum := sha256.Sum256(legacy)
			migration.Legacy = &database.LegacyMigration{
				Name:     "000001_create_example.up.sql",
				Checksum: fmt.Sprintf("%x", checksum), Tables: []string{"m_example"},
			}
		}
		result = append(result, migration)
	}
	return result, nil
}
