package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// testDatabases opens SQLite, plus PostgreSQL when TEST_DATABASE_URL is set.
// Each PostgreSQL test gets its own schema so runs do not share tables.
func testDatabases(t *testing.T) map[string]*gorm.DB {
	t.Helper()
	ctx := context.Background()
	dbs := map[string]*gorm.DB{}
	sqlite, err := Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Close(sqlite) })
	dbs["sqlite"] = sqlite
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		return dbs
	}
	schema := fmt.Sprintf("test_%s", strings.ToLower(strings.NewReplacer("/", "_", " ", "_", "-", "_").Replace(t.Name())))
	pg, err := Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`).Error; err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
		t.Fatal(err)
	}
	pgDB, _ := pg.DB()
	pgDB.SetMaxOpenConns(1) // keep search_path on the one connection
	if err := pg.Exec(`SET search_path TO "` + schema + `"`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pg.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`).Error
		_ = Close(pg)
	})
	dbs["postgres"] = pg
	return dbs
}

func TestConstraintClassifiesDriverErrors(t *testing.T) {
	for name, db := range testDatabases(t) {
		t.Run(name, func(t *testing.T) {
			for _, stmt := range []string{
				"CREATE TABLE parents (id TEXT PRIMARY KEY, email TEXT UNIQUE)",
				"CREATE TABLE children (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL REFERENCES parents(id), qty INTEGER CHECK (qty > 0))",
				"INSERT INTO parents VALUES ('p1', 'a@example.com')",
			} {
				if err := db.Exec(stmt).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range []struct {
				stmt string
				want ConstraintKind
			}{
				{"INSERT INTO parents VALUES ('p1', 'b@example.com')", ConstraintUnique},
				{"INSERT INTO parents VALUES ('p2', 'a@example.com')", ConstraintUnique},
				{"INSERT INTO children VALUES ('c1', 'missing', 1)", ConstraintForeignKey},
				{"INSERT INTO children VALUES ('c1', 'p1', 0)", ConstraintCheck},
			} {
				err := db.Exec(tc.stmt).Error
				if got := Constraint(fmt.Errorf("wrapped: %w", err)); got != tc.want {
					t.Errorf("%s: got %d, want %d (%v)", tc.stmt, got, tc.want, err)
				}
			}
			if Constraint(errors.New("other")) != ConstraintNone || Constraint(nil) != ConstraintNone {
				t.Fatal("unrelated error classified as a constraint")
			}
		})
	}
}
