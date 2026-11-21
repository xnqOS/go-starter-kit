package database

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"gorm.io/gorm"
)

type Migration struct {
	ID       string
	Checksum string // SHA-256 of the embedded, frozen Go source.
	Up       func(*gorm.DB) error
	Legacy   *LegacyMigration
}

// LegacyMigration recognizes the original SQL runner's history. It is a
// compatibility baseline, never an instruction to execute old SQL again.
type LegacyMigration struct {
	Name, Checksum string
	Tables         []string
}

type schemaMigration struct {
	ID        string    `gorm:"primaryKey;type:text"`
	Checksum  string    `gorm:"not null;type:text"`
	AppliedAt time.Time `gorm:"not null"`
}

func (schemaMigration) TableName() string { return "starter_go_migrations" }

var migrationID = regexp.MustCompile(`^[0-9]{6}_[a-z][a-z0-9_]*$`)
var checksumPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Migrate applies an append-only list in one transaction. PostgreSQL uses an
// advisory lock; SQLite connections opened by Open use BEGIN IMMEDIATE.
func Migrate(ctx context.Context, db *gorm.DB, migrations []Migration) error {
	ordered := append([]Migration(nil), migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for i, migration := range ordered {
		if !migrationID.MatchString(migration.ID) || !checksumPattern.MatchString(migration.Checksum) || migration.Up == nil {
			return fmt.Errorf("invalid migration definition: %s", migration.ID)
		}
		if i > 0 && ordered[i-1].ID[:6] == migration.ID[:6] {
			return fmt.Errorf("duplicate migration version: %s", migration.ID[:6])
		}
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(823401527)").Error; err != nil {
				return err
			}
		}
		if !tx.Migrator().HasTable(&schemaMigration{}) {
			if err := tx.Migrator().CreateTable(&schemaMigration{}); err != nil {
				return err
			}
		}
		var applied []schemaMigration
		if err := tx.Order("id ASC").Find(&applied).Error; err != nil {
			return err
		}
		if len(applied) > len(ordered) {
			return fmt.Errorf("database contains unknown migrations; use the matching application version")
		}
		for i, row := range applied {
			if row.ID != ordered[i].ID {
				return fmt.Errorf("migration history is not a prefix of this build: %s; do not remove or insert old migrations", row.ID)
			}
			if row.Checksum != ordered[i].Checksum {
				return fmt.Errorf("applied migration %s has changed; add a new migration instead", row.ID)
			}
		}
		legacy, err := legacyHistory(tx, ordered)
		if err != nil {
			return err
		}
		for _, migration := range ordered[len(applied):] {
			if legacy[migration.ID] {
				for _, table := range migration.Legacy.Tables {
					if !tx.Migrator().HasTable(table) {
						return fmt.Errorf("legacy migration %s is recorded but table %s is missing", migration.ID, table)
					}
				}
			} else if err := migration.Up(tx); err != nil {
				return fmt.Errorf("apply %s: %w", migration.ID, err)
			}
			if err := tx.Create(&schemaMigration{ID: migration.ID, Checksum: migration.Checksum, AppliedAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func legacyHistory(tx *gorm.DB, migrations []Migration) (map[string]bool, error) {
	result := make(map[string]bool)
	if !tx.Migrator().HasTable("starter_schema_migrations") {
		return result, nil
	}
	var rows []struct{ Name, Checksum string }
	if err := tx.Table("starter_schema_migrations").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		found := false
		for _, migration := range migrations {
			if migration.Legacy == nil || migration.Legacy.Name != row.Name {
				continue
			}
			if migration.Legacy.Checksum != row.Checksum {
				return nil, fmt.Errorf("legacy migration %s has an unexpected checksum", row.Name)
			}
			result[migration.ID], found = true, true
			break
		}
		if !found {
			return nil, fmt.Errorf("legacy migration %s needs an explicit Go migration baseline before upgrading", row.Name)
		}
	}
	return result, nil
}
