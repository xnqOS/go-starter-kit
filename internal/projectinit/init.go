// Package projectinit renames a project or exports a clean copy of the starter.
package projectinit

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var moduleDirective = regexp.MustCompile(`(?m)^module[\t ]+("[^"\r\n]+"|[^\t \r\n]+)[\t ]*\r?$`)

// The template builds against its own kit/ directory. A new project depends on
// the published kit version instead, so the local replace is dropped.
var localKitReplace = regexp.MustCompile(`(?m)\n*^replace[\t ]+github\.com/xnqOS/go-starter-kit/kit[\t ]+=>[\t ]+\./kit[\t ]*\r?$\n?`)

// Export only project sources and documentation, never arbitrary local files.
var projectEntries = []string{
	".env.example", ".env.deploy.example", ".github", ".gitignore", ".dockerignore",
	"Dockerfile", "Makefile", "README.md", "go.mod", "go.sum", "compose.yaml", "compose.deploy.yaml",
	"cmd", "internal", "migrations", "scripts", "docs", "deploy",
	"LICENSE", "LICENSE.md", "LICENSE.txt", "NOTICE", "CHANGELOG.md",
}

type file struct {
	path string
	body []byte
	mode fs.FileMode
}

// Initialize changes only go.mod and Go import declarations in place. With a
// destination it exports project files to a directory that must not yet exist.
func Initialize(root, module, destination string) (string, error) {
	if err := validateModule(module); err != nil {
		return "", err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if destination == "" {
		lockPath := filepath.Join(root, ".projectinit.lock")
		lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", fmt.Errorf("cannot acquire .projectinit.lock: %w", err)
		}
		lock.Close()
		defer os.Remove(lockPath)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	match := moduleDirective.FindSubmatchIndex(mod)
	if match == nil {
		return "", fmt.Errorf("go.mod must contain a module directive on its own line")
	}
	oldModule := string(mod[match[2]:match[3]])
	if strings.HasPrefix(oldModule, `"`) {
		oldModule, err = strconv.Unquote(oldModule)
		if err != nil {
			return "", err
		}
	}
	files, err := collect(root)
	if err != nil {
		return "", err
	}
	var changes []file
	for i := range files {
		original := files[i].body
		switch {
		case files[i].path == "go.mod":
			files[i].body = append(append([]byte{}, mod[:match[2]]...), []byte(module)...)
			files[i].body = append(files[i].body, mod[match[3]:]...)
			files[i].body = dropLocalKit(files[i].body)
		case strings.HasSuffix(files[i].path, ".go"):
			files[i].body, err = rewriteImports(files[i].path, original, oldModule, module)
			if err != nil {
				return "", err
			}
		}
		if !bytes.Equal(original, files[i].body) {
			changes = append(changes, files[i])
		}
	}
	if destination != "" {
		return export(root, destination, files)
	}
	if err := replaceFiles(root, changes); err != nil {
		return "", err
	}
	return root, nil
}

// dropLocalKit removes the template's replace of the kit module, keeping the
// file ending in one newline.
func dropLocalKit(mod []byte) []byte {
	loc := localKitReplace.FindIndex(mod)
	if loc == nil {
		return mod
	}
	rest := bytes.TrimLeft(mod[loc[1]:], "\r\n")
	out := append(append([]byte{}, mod[:loc[0]]...), '\n')
	if len(rest) > 0 {
		out = append(out, '\n')
	}
	return append(out, rest...)
}

func validateModule(module string) error {
	if module == "" || strings.ContainsAny(module, `\ :"'@$`+"\t\r\n") {
		return fmt.Errorf("MODULE must be a Go module path, e.g. github.com/acme/my-api")
	}
	for _, part := range strings.Split(module, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") {
			return fmt.Errorf("invalid module path %q", module)
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_~", r)) {
				return fmt.Errorf("invalid module path %q", module)
			}
		}
	}
	return nil
}

func collect(root string) ([]file, error) {
	var files []file
	for _, entry := range projectEntries {
		if _, err := os.Lstat(filepath.Join(root, entry)); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(filepath.Join(root, entry), func(path string, item fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if excluded(item.Name()) {
				if item.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if item.IsDir() {
				return nil
			}
			info, err := item.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("project file must be a regular file: %s", rel)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files = append(files, file{rel, body, info.Mode().Perm()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func excluded(name string) bool {
	if strings.HasPrefix(name, ".env") && name != ".env.example" && name != ".env.deploy.example" {
		return true
	}
	switch name {
	case ".git", ".aws", ".codex", ".agents", "certs", "data", "bin", "vendor", "node_modules", "__pycache__":
		return true
	}
	for _, suffix := range []string{".db", ".db-shm", ".db-wal", ".sqlite", ".sqlite3", ".pem", ".key", ".p12", ".tmp", ".lock"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return name == ".DS_Store"
}

func rewriteImports(path string, body []byte, old, next string) ([]byte, error) {
	set := token.NewFileSet()
	parsed, err := parser.ParseFile(set, path, body, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	changed := false
	for i := len(parsed.Imports) - 1; i >= 0; i-- {
		literal := parsed.Imports[i].Path
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			return nil, err
		}
		if value != old && !strings.HasPrefix(value, old+"/") {
			continue
		}
		start, end := set.Position(literal.Pos()).Offset, set.Position(literal.End()).Offset
		updated := append([]byte{}, body[:start]...)
		updated = append(updated, strconv.Quote(next+strings.TrimPrefix(value, old))...)
		body = append(updated, body[end:]...)
		changed = true
	}
	if !changed {
		return body, nil
	}
	// A new module path can change import sort order. Format only the header;
	// preserve bodies and unchanged migration files byte-for-byte.
	set = token.NewFileSet()
	parsed, err = parser.ParseFile(set, path, body, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	end := set.Position(parsed.Decls[len(parsed.Decls)-1].End()).Offset
	header, err := format.Source(body[:end])
	if err != nil {
		return nil, err
	}
	return append(bytes.TrimSuffix(header, []byte("\n")), body[end:]...), nil
}

func export(root, destination string, files []file) (_ string, result error) {
	destination, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	// Resolve the existing parent so aliases cannot put an export inside itself.
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return "", fmt.Errorf("destination parent must exist: %w", err)
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	rel, err := filepath.Rel(root, destination)
	if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("DEST must be outside the source project")
	}
	if err := os.Mkdir(destination, 0755); err != nil {
		return "", fmt.Errorf("DEST must be a new directory: %w", err)
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, os.RemoveAll(destination))
		}
	}()
	for _, item := range files {
		path := filepath.Join(destination, item.path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, item.body, item.mode); err != nil {
			return "", err
		}
	}
	return destination, nil
}

func replaceFiles(root string, changes []file) (result error) {
	var backups []file
	defer func() {
		if result != nil {
			for i := len(backups) - 1; i >= 0; i-- {
				item := backups[i]
				result = errors.Join(result, replace(filepath.Join(root, item.path), item.body, item.mode))
			}
		}
	}()
	for _, item := range changes {
		path := filepath.Join(root, item.path)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := replace(path, item.body, item.mode); err != nil {
			return err
		}
		backups = append(backups, file{item.path, body, item.mode})
	}
	return nil
}

func replace(path string, body []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".projectinit-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	_, writeErr := tmp.Write(body)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
