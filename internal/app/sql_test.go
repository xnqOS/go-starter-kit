package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
	"github.com/xnqOS/go-starter-kit/kit/database"
	"starter-kit/internal/config"
	"starter-kit/internal/example"
	"starter-kit/migrations"
)

// Each PostgreSQL test owns a separate schema; SQLite owns a temporary file.
// Both run the same repository, HTTP and migration assertions.
func sqlFixture(t *testing.T, storage string) (*gorm.DB, config.Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	cfg := config.Config{HTTPAddr: ":0", Storage: storage, DatabaseURL: filepath.Join(t.TempDir(), "nested", "test #1.db")}
	if storage == "postgres" {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
		}
		parsed, err := url.Parse(dsn)
		if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
			t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URL")
		}
		admin, err := database.Open(ctx, storage, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = database.Close(admin) })
		schema := "starter_test_" + strings.ToLower(rand.Text())
		if err := admin.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := admin.WithContext(cleanup).Exec(`DROP SCHEMA "` + schema + `" CASCADE`).Error; err != nil {
				t.Error(err)
			}
		})
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		cfg.DatabaseURL = parsed.String()
	}
	db, err := database.Open(ctx, storage, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(db); err != nil {
			t.Error(err)
		}
	})
	return db, cfg
}

func migrationList(t *testing.T) []database.Migration {
	t.Helper()
	list, err := migrations.List()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestSQLRepositoryAndApplication(t *testing.T) {
	for _, storage := range []string{"sqlite", "postgres"} {
		t.Run(storage, func(t *testing.T) {
			db, cfg := sqlFixture(t, storage)
			ctx := context.Background()
			list := migrationList(t)
			for range 2 {
				if err := database.Migrate(ctx, db, list); err != nil {
					t.Fatal(err)
				}
			}
			if storage == "sqlite" {
				var enabled int
				if err := db.Raw("PRAGMA foreign_keys").Scan(&enabled).Error; err != nil || enabled != 1 {
					t.Fatalf("foreign_keys: %d %v", enabled, err)
				}
				var journal string
				if err := db.Raw("PRAGMA journal_mode").Scan(&journal).Error; err != nil || journal != "wal" {
					t.Fatalf("journal: %s %v", journal, err)
				}
			}
			repo := example.NewSQLRepo(db)
			now := time.Now().UTC().Truncate(time.Microsecond)
			for _, id := range []string{"a", "b", "c"} {
				item := example.Example{ID: id, Name: strings.Repeat("好", 200), CreatedAt: now, UpdatedAt: now}
				if err := repo.Create(ctx, item); err != nil {
					t.Fatal(err)
				}
			}
			items, total, err := repo.List(ctx, example.ListParams{Limit: 1, Offset: 1})
			if err != nil || len(items) != 1 || items[0].ID != "b" || total != 3 {
				t.Fatalf("pagination: %#v total=%d %v", items, total, err)
			}
			if !items[0].CreatedAt.Equal(now) {
				t.Fatalf("timestamp round trip: %s != %s", items[0].CreatedAt, now)
			}
			if err := repo.Create(ctx, example.Example{ID: "too-long", Name: strings.Repeat("好", 201), CreatedAt: now, UpdatedAt: now}); err == nil {
				t.Fatal("database did not enforce name length")
			}
			if err := repo.Create(ctx, example.Example{ID: "a", Name: "duplicate", CreatedAt: now, UpdatedAt: now}); err == nil {
				t.Fatal("duplicate primary key accepted")
			}
			updated, err := repo.Update(ctx, "a", "updated", now.Add(time.Second))
			if err != nil || updated.ID != "a" || updated.Name != "updated" || !updated.CreatedAt.Equal(now) || !updated.UpdatedAt.Equal(now.Add(time.Second)) {
				t.Fatalf("update returning: %#v %v", updated, err)
			}
			for _, id := range []string{"missing", `x' OR '1'='1`} {
				if _, err := repo.Get(ctx, id); !errors.Is(err, apperror.ErrNotFound) {
					t.Fatalf("get missing: %v", err)
				}
				if _, err := repo.Update(ctx, id, "changed", now); !errors.Is(err, apperror.ErrNotFound) {
					t.Fatalf("update missing: %v", err)
				}
				if err := repo.Delete(ctx, id); !errors.Is(err, apperror.ErrNotFound) {
					t.Fatalf("delete missing: %v", err)
				}
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, _, err := repo.List(canceled, example.ListParams{Limit: 1}); err == nil {
				t.Fatal("canceled context ignored")
			}
			for _, id := range []string{"a", "b", "c"} {
				if err := repo.Delete(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			items, total, err = repo.List(ctx, example.ListParams{Limit: 10})
			if err != nil || items == nil || len(items) != 0 || total != 0 {
				t.Fatalf("empty list: %#v total=%d %v", items, total, err)
			}

			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			application, err := New(ctx, cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			send := func(method, path, body string, status int) []byte {
				t.Helper()
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				application.Handler.ServeHTTP(w, req)
				if w.Code != status {
					t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
				}
				return w.Body.Bytes()
			}
			send("GET", "/ready", "", 200)
			body := send("POST", "/api/v1/example", `{"name":"persistent"}`, 201)
			var created struct {
				Data example.Example `json:"data"`
			}
			if err := json.Unmarshal(body, &created); err != nil || created.Data.ID == "" {
				t.Fatalf("create: %s %v", body, err)
			}
			path := "/api/v1/example/" + created.Data.ID
			// Close and reopen the application: the same database must keep data.
			if err := application.Close(); err != nil {
				t.Fatal(err)
			}
			application, err = New(ctx, cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			send("GET", path, "", 200)
			body = send("GET", "/api/v1/example?limit=1&offset=99", "", 200)
			var page struct {
				Code       string            `json:"code"`
				Data       []example.Example `json:"data"`
				Pagination struct {
					Limit  int   `json:"limit"`
					Offset int   `json:"offset"`
					Total  int64 `json:"total"`
				} `json:"pagination"`
			}
			if err := json.Unmarshal(body, &page); err != nil || page.Code != "ok" || page.Data == nil || len(page.Data) != 0 || page.Pagination.Total != 1 || page.Pagination.Limit != 1 || page.Pagination.Offset != 99 {
				t.Fatalf("HTTP empty page: %s %v", body, err)
			}
			body = send("PUT", path, `{"name":"changed"}`, 200)
			var updatedHTTP struct {
				Data example.Example `json:"data"`
			}
			if err := json.Unmarshal(body, &updatedHTTP); err != nil || updatedHTTP.Data.Name != "changed" {
				t.Fatalf("HTTP update: %s %v", body, err)
			}
			send("DELETE", path, "", 204)
			send("GET", path, "", 404)
			send("PUT", path, `{"name":"missing"}`, 404)
			send("DELETE", path, "", 404)
		})
	}
}

type rollbackProbe struct {
	ID string `gorm:"primaryKey"`
}

func (rollbackProbe) TableName() string { return "rollback_probe" }

func extraMigration(id string, up func(*gorm.DB) error) database.Migration {
	return database.Migration{ID: id, Checksum: fmt.Sprintf("%x", sha256.Sum256([]byte(id))), Up: up}
}

func TestSQLMigrationIntegrityAndRollback(t *testing.T) {
	for _, storage := range []string{"sqlite", "postgres"} {
		t.Run(storage, func(t *testing.T) {
			db, _ := sqlFixture(t, storage)
			ctx := context.Background()
			list := migrationList(t)
			if err := database.Migrate(ctx, db, list); err != nil {
				t.Fatal(err)
			}
			changed := append([]database.Migration(nil), list...)
			changed[0].Checksum = strings.Repeat("0", 64)
			if err := database.Migrate(ctx, db, changed); err == nil {
				t.Fatal("edited migration accepted")
			}
			if err := database.Migrate(ctx, db, nil); err == nil {
				t.Fatal("unknown applied migration accepted")
			}
			broken := append(append([]database.Migration(nil), list...),
				extraMigration("900001_probe", func(tx *gorm.DB) error { return tx.Migrator().CreateTable(&rollbackProbe{}) }),
				extraMigration("900002_broken", func(tx *gorm.DB) error { return tx.Exec("THIS IS NOT SQL").Error }),
			)
			if err := database.Migrate(ctx, db, broken); err == nil {
				t.Fatal("invalid migration accepted")
			}
			if db.Migrator().HasTable(&rollbackProbe{}) {
				t.Fatal("failed batch left earlier DDL")
			}
			var count int64
			if err := db.Table("starter_go_migrations").Count(&count).Error; err != nil || count != int64(len(list)) {
				t.Fatalf("failed batch changed history: %d %v", count, err)
			}
			// A later successful version must prevent insertion before it.
			later := append(append([]database.Migration(nil), list...), extraMigration("900003_later", func(tx *gorm.DB) error { return tx.Migrator().CreateTable(&rollbackProbe{}) }))
			if err := database.Migrate(ctx, db, later); err != nil {
				t.Fatal(err)
			}
			inserted := append(append([]database.Migration(nil), later...), extraMigration("900002_inserted", func(*gorm.DB) error { return nil }))
			if err := database.Migrate(ctx, db, inserted); err == nil {
				t.Fatal("out-of-order migration accepted")
			}
		})
	}
}

func TestSQLConcurrentMigrations(t *testing.T) {
	for _, storage := range []string{"sqlite", "postgres"} {
		t.Run(storage, func(t *testing.T) {
			db, cfg := sqlFixture(t, storage)
			second, err := database.Open(context.Background(), storage, cfg.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close(second)
			list := migrationList(t)
			results := make(chan error, 2)
			for _, connection := range []*gorm.DB{db, second} {
				go func() { results <- database.Migrate(context.Background(), connection, list) }()
			}
			for range 2 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSQLLegacyBaselinePreservesExistingData(t *testing.T) {
	for _, storage := range []string{"sqlite", "postgres"} {
		t.Run(storage, func(t *testing.T) {
			db, _ := sqlFixture(t, storage)
			list := migrationList(t)
			if storage == "postgres" {
				body, err := os.ReadFile(filepath.Join("..", "..", "migrations", "legacy", list[0].Legacy.Name))
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Exec(string(body)).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := list[0].Up(db); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("CREATE TABLE starter_schema_migrations (name TEXT PRIMARY KEY, checksum TEXT NOT NULL)").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("INSERT INTO starter_schema_migrations(name,checksum) VALUES (?,?)", list[0].Legacy.Name, list[0].Legacy.Checksum).Error; err != nil {
				t.Fatal(err)
			}
			repo := example.NewSQLRepo(db)
			now := time.Now().UTC()
			if err := repo.Create(context.Background(), example.Example{ID: "existing", Name: "keep me", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := database.Migrate(context.Background(), db, list); err != nil {
					t.Fatal(err)
				}
			}
			item, err := repo.Get(context.Background(), "existing")
			if err != nil || item.Name != "keep me" {
				t.Fatalf("legacy data changed: %#v %v", item, err)
			}
			if err := db.Exec("INSERT INTO starter_schema_migrations(name,checksum) VALUES (?,?)", "000002_custom.up.sql", strings.Repeat("0", 64)).Error; err != nil {
				t.Fatal(err)
			}
			if err := database.Migrate(context.Background(), db, list); err == nil {
				t.Fatal("unknown legacy migration silently ignored")
			}
		})
	}
}

// A migration in another process holds the SQLite write lock. Listing must
// keep reading the last committed snapshot instead of waiting for the lock.
func TestSQLiteListDoesNotWaitForWriter(t *testing.T) {
	db, cfg := sqlFixture(t, "sqlite")
	ctx := context.Background()
	if err := database.Migrate(ctx, db, migrationList(t)); err != nil {
		t.Fatal(err)
	}
	repo := example.NewSQLRepo(db)
	now := time.Now().UTC()
	for _, id := range []string{"a", "b"} {
		if err := repo.Create(ctx, example.Example{ID: id, Name: id, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	writer, err := database.Open(ctx, "sqlite", cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(writer) })
	tx := writer.Begin() // BEGIN IMMEDIATE: takes the write lock
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	if err := tx.Exec("INSERT INTO m_example (id, name, created_at, updated_at) VALUES ('uncommitted', 'x', ?, ?)", now, now).Error; err != nil {
		t.Fatal(err)
	}

	for _, params := range []example.ListParams{{Limit: 1}, {Limit: 10}} { // with and without COUNT
		listCtx, cancel := context.WithTimeout(ctx, time.Second)
		start := time.Now()
		items, total, err := repo.List(listCtx, params)
		cancel()
		if err != nil || total != 2 || time.Since(start) > 500*time.Millisecond {
			t.Fatalf("list %+v while a writer holds the lock: %d items, total=%d, %s, %v", params, len(items), total, time.Since(start), err)
		}
	}
}

// docs/database.md shares one transaction across repositories with
// NewSQLRepo(tx). List must run on that transaction: on SQLite's single
// connection it would otherwise wait forever, and on a separate connection it
// would not see the transaction's own uncommitted rows.
func TestSQLListInsideOuterTransaction(t *testing.T) {
	for _, storage := range []string{"sqlite", "postgres"} {
		t.Run(storage, func(t *testing.T) {
			db, _ := sqlFixture(t, storage)
			ctx := context.Background()
			if err := database.Migrate(ctx, db, migrationList(t)); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			sentinel := errors.New("roll back")
			err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				repo := example.NewSQLRepo(tx)
				for _, id := range []string{"a", "b"} {
					if err := repo.Create(ctx, example.Example{ID: id, Name: id, CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
				}
				for _, params := range []example.ListParams{{Limit: 1}, {Limit: 10}} { // with and without COUNT
					listCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
					items, total, err := repo.List(listCtx, params)
					cancel()
					if err != nil || total != 2 || len(items) != min(params.Limit, 2) {
						t.Errorf("list %+v inside transaction: %d items, total=%d, %v", params, len(items), total, err)
					}
				}
				return sentinel
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("transaction: %v", err)
			}
		})
	}
}
