package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureWiring = `package app
import (
    "github.com/gin-gonic/gin"
    "gorm.io/gorm"
    // scaffold:imports
)
func registerModules(api *gin.RouterGroup, db *gorm.DB) {
    // scaffold:modules
}
`

const fixtureRegistry = `package migrations
import "gorm.io/gorm"
func definitions() {
    _ = []struct { id string; up func(*gorm.DB) error }{
        // scaffold:migrations
    }
}
`

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"internal/app", "migrations"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, root, "go.mod", "module example.test/backend\n\ngo 1.25.0\n")
	write(t, root, "internal/app/modules.go", fixtureWiring)
	write(t, root, "migrations/migrations.go", fixtureRegistry)
	return root
}

func write(t *testing.T, root, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestGenerateRegistersModuleAndUsesProjectModulePath(t *testing.T) {
	root := fixture(t)
	if _, err := Generate(root, Options{Name: "coffee_fund", Path: "/coffee-funds"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"internal/coffee_fund/handler.go", "internal/coffee_fund/routes.go",
		"internal/coffee_fund/service_test.go", "internal/coffee_fund/handler_test.go",
		"internal/coffee_fund/repo_sql.go", "internal/coffee_fund/repo_memory.go",
		"migrations/000001_create_coffee_fund.go",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	wiring := read(t, root, "internal/app/modules.go")
	for _, want := range []string{`"example.test/backend/internal/coffee_fund"`, `api.Group("/coffee-funds")`, "NewMemoryRepo()", "NewSQLRepo(db)"} {
		if !strings.Contains(wiring, want) {
			t.Fatalf("missing %s in wiring", want)
		}
	}
	if body := read(t, root, "internal/coffee_fund/service.go"); !strings.Contains(body, `"github.com/xnqOS/go-starter-kit/kit/apperror"`) {
		t.Fatal("generated imports did not use the shared kit module")
	}
	if _, err := Generate(root, Options{Name: "order"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "migrations/000002_create_order.go")); err != nil {
		t.Fatal(err)
	}
	registry := read(t, root, "migrations/migrations.go")
	for _, entry := range []string{
		`{"000001_create_coffee_fund", createCoffeeFundV000001}`,
		`{"000002_create_order", createOrderV000002}`,
	} {
		if !strings.Contains(registry, entry) {
			t.Fatalf("missing migration registration %s", entry)
		}
	}
	snapshot := read(t, root, "migrations/000001_create_coffee_fund.go")
	if strings.Contains(snapshot, "internal/coffee_fund") || !strings.Contains(snapshot, "coffee_fundV000001") {
		t.Fatal("migration must own a frozen schema snapshot")
	}
	if _, err := Generate(root, Options{Name: "coffee_fund"}); err == nil {
		t.Fatal("overwrote existing module")
	}
	if _, err := Generate(root, Options{Name: "other", Path: "/coffee-funds"}); err == nil {
		t.Fatal("accepted duplicate route")
	}
}

func TestGenerateRejectsInvalidNamesAndPaths(t *testing.T) {
	for _, name := range []string{"", "../escape", "user-name", "type", "main", "Upper", "_hidden", strings.Repeat("a", 41)} {
		t.Run(name, func(t *testing.T) {
			if _, err := Generate(fixture(t), Options{Name: name}); err == nil {
				t.Fatalf("accepted %q", name)
			}
		})
	}
	for _, path := range []string{"/", "/nested/path", "/:id", "/hello?query=1", "orders"} {
		if _, err := Generate(fixture(t), Options{Name: "order", Path: path}); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
}

func TestDryRunAndMissingMarkersDoNotWriteModule(t *testing.T) {
	root := fixture(t)
	if _, err := Generate(root, Options{Name: "order", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "internal/order")); !os.IsNotExist(err) {
		t.Fatal("dry run created module")
	}
	if _, err := os.Stat(filepath.Join(root, ".modulegen.lock")); !os.IsNotExist(err) {
		t.Fatal("dry run created lock")
	}
	if read(t, root, "internal/app/modules.go") != fixtureWiring {
		t.Fatal("dry run changed wiring")
	}
	if read(t, root, "migrations/migrations.go") != fixtureRegistry {
		t.Fatal("dry run changed migration registry")
	}
	write(t, root, "internal/app/modules.go", strings.ReplaceAll(fixtureWiring, "// scaffold:modules", ""))
	if _, err := Generate(root, Options{Name: "order"}); err == nil {
		t.Fatal("accepted missing wiring marker")
	}
	if _, err := os.Stat(filepath.Join(root, "internal/order")); !os.IsNotExist(err) {
		t.Fatal("failed generation left module")
	}
}

func TestWriteFailureRollsBackWithoutOverwritingExistingFiles(t *testing.T) {
	root := fixture(t)
	write(t, root, "migrations/conflict.go", "existing migration")
	files := []outputFile{
		{"internal/order/model.go", []byte("package order")},
		{"migrations/conflict.go", []byte("replacement")},
	}
	if err := writeFiles(root, filepath.Join(root, "internal/order"), files, nil); err == nil {
		t.Fatal("overwrote an existing migration")
	}
	if _, err := os.Stat(filepath.Join(root, "internal/order")); !os.IsNotExist(err) {
		t.Fatal("failed generation left module")
	}
	if read(t, root, "migrations/conflict.go") != "existing migration" {
		t.Fatal("existing migration changed")
	}
	if read(t, root, "internal/app/modules.go") != fixtureWiring {
		t.Fatal("failed generation changed wiring")
	}
}

func TestRegistryWriteFailureRestoresWiringAndRemovesNewFiles(t *testing.T) {
	root := fixture(t)
	files := []outputFile{{"internal/order/model.go", []byte("package order")}}
	changes := []outputFile{
		{"internal/app/modules.go", []byte("package app")},
		{"missing/registry.go", []byte("package migrations")},
	}
	if err := writeFiles(root, filepath.Join(root, "internal/order"), files, changes); err == nil {
		t.Fatal("expected registry write failure")
	}
	if read(t, root, "internal/app/modules.go") != fixtureWiring {
		t.Fatal("wiring was not restored")
	}
	if _, err := os.Stat(filepath.Join(root, "internal/order")); !os.IsNotExist(err) {
		t.Fatal("failed update left generated module")
	}
}

func TestMissingMigrationMarkerDoesNotWrite(t *testing.T) {
	root := fixture(t)
	write(t, root, "migrations/migrations.go", "package migrations")
	if _, err := Generate(root, Options{Name: "order"}); err == nil {
		t.Fatal("missing marker accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "internal/order")); !os.IsNotExist(err) {
		t.Fatal("module was written")
	}
	if read(t, root, "internal/app/modules.go") != fixtureWiring {
		t.Fatal("wiring changed")
	}
}
