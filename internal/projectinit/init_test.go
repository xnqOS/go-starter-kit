package projectinit

import (
	"bytes"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenameKeepsImportsFormattedAndBodyUnchanged(t *testing.T) {
	body := []byte("package main\n\nimport (\n\t\"github.com/a/library\"\n\t\"starter-kit/internal/app\"\n)\n\n// Preserve this body exactly.\nvar message=\"starter-kit/internal/app\"\n")
	updated, err := rewriteImports("main.go", body, "starter-kit", "example.com/shop")
	if err != nil {
		t.Fatal(err)
	}
	suffix := []byte("\n\n// Preserve this body exactly.\nvar message=\"starter-kit/internal/app\"\n")
	if !bytes.HasSuffix(updated, suffix) {
		t.Fatal("rename changed the source body")
	}
	header := bytes.TrimSuffix(updated, suffix)
	formatted, err := format.Source(header)
	if err != nil || !bytes.Equal(bytes.TrimSuffix(formatted, []byte("\n")), header) {
		t.Fatalf("renamed imports not formatted: %s %v", updated, err)
	}
}

func put(t *testing.T, root, path, body string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func contents(t *testing.T, root, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	put(t, root, "go.mod", "module starter-kit\n\ngo 1.25.0\n", 0644)
	put(t, root, "cmd/api/main.go", `package main
import (
    alias "starter-kit/internal/app"
    _ "starter-kit-extra/other"
)
// starter-kit/internal/app is mentioned in a comment.
const message = "starter-kit/internal/app"
`, 0644)
	put(t, root, "migrations/000001_example.go", "package migrations\n// Frozen bytes must not change.\n", 0644)
	put(t, root, "scripts/run.sh", "#!/bin/sh\n", 0755)
	return root
}

func TestRenameOnlyChangesModuleAndImportsAndCanBeRepeated(t *testing.T) {
	root := fixture(t)
	put(t, root, ".env", "LOG_SERVICE=existing\n", 0600)
	put(t, root, "other-project/reference.go", "not Go source", 0644)
	const next = "github.com/acme/orders/v2"
	for range 2 {
		if _, err := Initialize(root, next, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := contents(t, root, "go.mod"); got != "module "+next+"\n\ngo 1.25.0\n" {
		t.Fatal(got)
	}
	body := contents(t, root, "cmd/api/main.go")
	for _, want := range []string{
		`alias "github.com/acme/orders/v2/internal/app"`,
		`_ "starter-kit-extra/other"`,
		`const message = "starter-kit/internal/app"`,
		"// starter-kit/internal/app is mentioned in a comment.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	if got := contents(t, root, "migrations/000001_example.go"); got != "package migrations\n// Frozen bytes must not change.\n" {
		t.Fatal("unchanged migration was rewritten")
	}
	if got := contents(t, root, ".env"); got != "LOG_SERVICE=existing\n" {
		t.Fatal("local config changed")
	}
}

func TestExportOmitsLocalFilesAndKeepsExecutableScripts(t *testing.T) {
	root := fixture(t)
	for _, path := range []string{
		".env", ".env.deploy", ".env.local", ".git/config", ".aws/credentials", "bin/api", "data/app.db",
		"other-project/go.mod", "another-project/go.mod", "deploy/log-server/certs/server.key",
		"internal/test.db", "scripts/.env.secret", "deploy/private.pem", "unknown-local-file.txt",
	} {
		put(t, root, path, "local data", 0600)
	}
	put(t, root, ".env.example", "STORAGE=sqlite\n", 0644)
	put(t, root, ".env.deploy.example", "STORAGE=sqlite\n", 0644)
	put(t, root, ".github/workflows/test.yml", "name: Test\n", 0644)
	destination := filepath.Join(t.TempDir(), "new-project")
	if _, err := Initialize(root, "example.com/new-project", destination); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".env", ".env.deploy", ".env.local", ".git", ".aws", "bin", "data", "other-project", "another-project", "deploy/log-server/certs", "internal/test.db", "scripts/.env.secret", "deploy/private.pem", "unknown-local-file.txt"} {
		if _, err := os.Lstat(filepath.Join(destination, path)); !os.IsNotExist(err) {
			t.Fatalf("exported %s", path)
		}
	}
	for _, path := range []string{".env.example", ".env.deploy.example", ".github/workflows/test.yml"} {
		if _, err := os.Stat(filepath.Join(destination, path)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(destination, "scripts/run.sh"))
	if err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatal("script lost executable permissions")
	}
	if !strings.HasPrefix(contents(t, root, "go.mod"), "module starter-kit\n") {
		t.Fatal("export modified source")
	}
}

func TestInitializeDropsLocalKitReplace(t *testing.T) {
	root := fixture(t)
	mod := "module starter-kit\n\ngo 1.25.0\n\nrequire github.com/xnqOS/go-starter-kit/kit v0.1.0\n\nreplace github.com/xnqOS/go-starter-kit/kit => ./kit\n"
	put(t, root, "go.mod", mod, 0644)
	put(t, root, "kit/go.mod", "module github.com/xnqOS/go-starter-kit/kit\n", 0644)
	destination := filepath.Join(t.TempDir(), "new-project")
	if _, err := Initialize(root, "example.com/new-project", destination); err != nil {
		t.Fatal(err)
	}
	want := "module example.com/new-project\n\ngo 1.25.0\n\nrequire github.com/xnqOS/go-starter-kit/kit v0.1.0\n"
	if got := contents(t, destination, "go.mod"); got != want {
		t.Fatalf("go.mod = %q, want %q", got, want)
	}
	if _, err := os.Lstat(filepath.Join(destination, "kit")); !os.IsNotExist(err) {
		t.Fatal("exported the template's kit directory")
	}
	if got := contents(t, root, "go.mod"); got != mod {
		t.Fatal("export modified source go.mod")
	}
	// A replace in the middle keeps the following directives separated.
	middle := "module starter-kit\n\nreplace github.com/xnqOS/go-starter-kit/kit => ./kit\n\nrequire x.test/y v1.0.0\n"
	if got, want := string(dropLocalKit([]byte(middle))), "module starter-kit\n\nrequire x.test/y v1.0.0\n"; got != want {
		t.Fatalf("dropLocalKit = %q, want %q", got, want)
	}
}

func TestRejectsInvalidInputWithoutWriting(t *testing.T) {
	root := fixture(t)
	for _, module := range []string{"", "../escape", "a//b", "/absolute", "a/..", "a b", "a\nb", "a@v1", `a\b`, "a/$(touch x)"} {
		if _, err := Initialize(root, module, ""); err == nil {
			t.Fatalf("accepted %q", module)
		}
	}
	for _, destination := range []string{root, filepath.Join(root, "child"), t.TempDir()} {
		if _, err := Initialize(root, "example.com/new", destination); err == nil {
			t.Fatalf("accepted destination %s", destination)
		}
	}
	if !strings.HasPrefix(contents(t, root, "go.mod"), "module starter-kit\n") {
		t.Fatal("failed initialization modified source")
	}
}

func TestRejectsSymlinksAndInvalidGoBeforeChanges(t *testing.T) {
	root := fixture(t)
	if err := os.Mkdir(filepath.Join(root, "internal"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "go.mod"), filepath.Join(root, "internal", "link.go")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "new")
	if _, err := Initialize(root, "example.com/new", destination); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("failed preflight created destination")
	}
	if err := os.Remove(filepath.Join(root, "internal", "link.go")); err != nil {
		t.Fatal(err)
	}
	put(t, root, "internal/invalid.go", "not Go source", 0644)
	if _, err := Initialize(root, "example.com/new", ""); err == nil {
		t.Fatal("accepted invalid Go source")
	}
	if !strings.HasPrefix(contents(t, root, "go.mod"), "module starter-kit\n") {
		t.Fatal("preflight failure modified go.mod")
	}
}

func TestWriteFailureRestoresPreviouslyChangedFiles(t *testing.T) {
	root := fixture(t)
	original := contents(t, root, "go.mod")
	changes := []file{
		{"go.mod", []byte("module example.com/new\n"), 0644},
		{"missing/file.go", []byte("package missing\n"), 0644},
	}
	if err := replaceFiles(root, changes); err == nil {
		t.Fatal("accepted missing destination file")
	}
	if contents(t, root, "go.mod") != original {
		t.Fatal("write failure did not restore go.mod")
	}
}
