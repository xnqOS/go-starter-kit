// Package scaffold generates editable business modules and explicit wiring.
package scaffold

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templates embed.FS

var (
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z][a-z0-9]*)*$`)
	routePattern   = regexp.MustCompile(`^/[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	versionPattern = regexp.MustCompile(`^(\d{6})_.*\.go$`)
)

type Options struct {
	Name   string
	Path   string
	DryRun bool
}

type moduleData struct{ Module, Package, Type, Alias, Table, Path, Version string }
type outputFile struct {
	path string
	body []byte
}

func Generate(root string, options Options) ([]string, error) {
	if len(options.Name) > 40 || !namePattern.MatchString(options.Name) || token.Lookup(options.Name).IsKeyword() || options.Name == "main" {
		return nil, fmt.Errorf("name must be a non-keyword Go package name of at most 40 characters, such as order or coffee_fund")
	}
	if options.Path == "" {
		options.Path = "/" + strings.ReplaceAll(options.Name, "_", "-")
	}
	if !routePattern.MatchString(options.Path) {
		return nil, fmt.Errorf("path must be one literal route segment, such as /orders or /coffee-fund")
	}
	if !options.DryRun {
		lockPath := filepath.Join(root, ".modulegen.lock")
		lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, fmt.Errorf("cannot acquire .modulegen.lock; another generator may be running: %w", err)
		}
		lock.Close()
		defer os.Remove(lockPath)
	}
	modulePath, err := readModule(root)
	if err != nil {
		return nil, err
	}
	moduleDir := filepath.Join(root, "internal", options.Name)
	if _, err := os.Lstat(moduleDir); !os.IsNotExist(err) {
		return nil, fmt.Errorf("module directory already exists or is inaccessible: internal/%s", options.Name)
	}
	wiringPath := filepath.Join(root, "internal", "app", "modules.go")
	wiring, err := os.ReadFile(wiringPath)
	if err != nil {
		return nil, err
	}
	for _, marker := range []string{"// scaffold:imports", "// scaffold:modules"} {
		if bytes.Count(wiring, []byte(marker)) != 1 {
			return nil, fmt.Errorf("modules.go must contain exactly one %s marker", marker)
		}
	}
	if err := checkRoute(wiring, options.Path); err != nil {
		return nil, err
	}
	registryPath := filepath.Join(root, "migrations", "migrations.go")
	registry, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, err
	}
	if bytes.Count(registry, []byte("// scaffold:migrations")) != 1 {
		return nil, fmt.Errorf("migrations.go must contain exactly one // scaffold:migrations marker")
	}
	version, err := nextVersion(filepath.Join(root, "migrations"))
	if err != nil {
		return nil, err
	}
	typeName := exportedName(options.Name)
	data := moduleData{modulePath, options.Name, typeName, "module" + typeName, "m_" + options.Name, options.Path, fmt.Sprintf("%06d", version)}
	files, err := renderFiles(data, version)
	if err != nil {
		return nil, err
	}
	importLine := fmt.Sprintf("%s %q\n\t// scaffold:imports", data.Alias, data.Module+"/internal/"+data.Package)
	wiring = bytes.Replace(wiring, []byte("// scaffold:imports"), []byte(importLine), 1)
	registration := fmt.Sprintf(`{
		var repo %[1]s.Repo = %[1]s.NewMemoryRepo()
		if db != nil { repo = %[1]s.NewSQLRepo(db) }
		%[1]s.RegisterRoutes(api.Group(%[2]q), %[1]s.NewService(repo))
	}
	// scaffold:modules`, data.Alias, data.Path)
	wiring = bytes.Replace(wiring, []byte("// scaffold:modules"), []byte(registration), 1)
	wiring, err = format.Source(wiring)
	if err != nil {
		return nil, fmt.Errorf("format wiring: %w", err)
	}
	entry := fmt.Sprintf("{%q, create%sV%s},\n// scaffold:migrations", data.Version+"_create_"+data.Package, data.Type, data.Version)
	registry = bytes.Replace(registry, []byte("// scaffold:migrations"), []byte(entry), 1)
	registry, err = format.Source(registry)
	if err != nil {
		return nil, fmt.Errorf("format migration registry: %w", err)
	}
	paths := make([]string, 0, len(files)+2)
	for _, file := range files {
		paths = append(paths, file.path)
	}
	paths = append(paths, filepath.Join("internal", "app", "modules.go"))
	paths = append(paths, filepath.Join("migrations", "migrations.go"))
	if options.DryRun {
		return paths, nil
	}
	changes := []outputFile{
		{filepath.Join("internal", "app", "modules.go"), wiring},
		{filepath.Join("migrations", "migrations.go"), registry},
	}
	if err := writeFiles(root, moduleDir, files, changes); err != nil {
		return nil, err
	}
	return paths, nil
}

func readModule(root string) (string, error) {
	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			value := fields[1]
			if strings.HasPrefix(value, `"`) {
				value, err = strconv.Unquote(value)
				if err != nil {
					return "", err
				}
			}
			return value, nil
		}
	}
	return "", fmt.Errorf("go.mod has no module directive")
}

func exportedName(name string) string {
	parts := strings.Split(name, "_")
	initialisms := map[string]string{"api": "API", "id": "ID", "url": "URL", "http": "HTTP"}
	for i, part := range parts {
		if initialism, ok := initialisms[part]; ok {
			parts[i] = initialism
		} else {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "")
}

func checkRoute(wiring []byte, route string) error {
	file, err := parser.ParseFile(token.NewFileSet(), "modules.go", wiring, 0)
	if err != nil {
		return err
	}
	var collision bool
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Group" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && value == route {
			collision = true
		}
		return true
	})
	if collision {
		return fmt.Errorf("route /api/v1%s is already registered", route)
	}
	return nil
}

func nextVersion(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	version := 0
	for _, entry := range entries {
		match := versionPattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		n, _ := strconv.Atoi(match[1])
		version = max(version, n)
	}
	if version == 999999 {
		return 0, fmt.Errorf("migration sequence exhausted")
	}
	return version + 1, nil
}

func renderFiles(data moduleData, version int) ([]outputFile, error) {
	entries, err := templates.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	files := make([]outputFile, 0, len(entries))
	for _, entry := range entries {
		body, err := templates.ReadFile("templates/" + entry.Name())
		if err != nil {
			return nil, err
		}
		tmpl, err := template.New(entry.Name()).Option("missingkey=error").Parse(string(body))
		if err != nil {
			return nil, err
		}
		var buffer bytes.Buffer
		if err := tmpl.Execute(&buffer, data); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(entry.Name(), ".tmpl")
		content := buffer.Bytes()
		path := filepath.Join("internal", data.Package, name)
		if name == "migration.go" {
			path = filepath.Join("migrations", fmt.Sprintf("%06d_create_%s.go", version, data.Package))
		}
		content, err = format.Source(content)
		if err != nil {
			return nil, fmt.Errorf("format %s: %w", path, err)
		}
		files = append(files, outputFile{path, content})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func writeFiles(root, moduleDir string, files, changes []outputFile) (result error) {
	if err := os.Mkdir(moduleDir, 0755); err != nil {
		return err
	}
	created := []string{}
	backups := []outputFile{}
	defer func() {
		if result != nil {
			for i := len(backups) - 1; i >= 0; i-- {
				if err := replaceFile(filepath.Join(root, backups[i].path), backups[i].body); err != nil {
					result = errors.Join(result, fmt.Errorf("restore %s: %w", backups[i].path, err))
				}
			}
			for _, path := range created {
				_ = os.Remove(path)
			}
			_ = os.Remove(moduleDir)
		}
	}()
	for _, file := range files {
		path := filepath.Join(root, file.path)
		out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		created = append(created, path)
		_, writeErr := out.Write(file.body)
		closeErr := out.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, change := range changes {
		path := filepath.Join(root, change.path)
		original, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := replaceFile(path, change.body); err != nil {
			return err
		}
		backups = append(backups, outputFile{change.path, original})
	}
	return nil
}

func replaceFile(path string, body []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".modulegen-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		temp.Close()
		return err
	}
	_, writeErr := temp.Write(body)
	closeErr := temp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), path)
}
