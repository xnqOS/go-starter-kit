package deployconfig

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var envMu sync.Mutex

// WithEnv runs fn with exactly values in the process environment: every name
// in names and in values is cleared first, non-empty values are set, and the
// previous environment is restored afterwards. Calls are serialized, but
// other goroutines reading the environment meanwhile see the temporary
// values, so use it only in tools and tests.
func WithEnv(values Values, names []string, fn func() error) error {
	envMu.Lock()
	defer envMu.Unlock()
	all := map[string]bool{}
	for _, name := range names {
		all[name] = true
	}
	for name := range values {
		all[name] = true
	}
	saved := map[string]*string{}
	for name := range all {
		if old, ok := os.LookupEnv(name); ok {
			saved[name] = &old
		} else {
			saved[name] = nil
		}
		os.Unsetenv(name)
	}
	defer func() {
		for name, old := range saved {
			if old == nil {
				os.Unsetenv(name)
			} else {
				os.Setenv(name, *old)
			}
		}
	}()
	for name, value := range values {
		if value != "" {
			os.Setenv(name, value)
		}
	}
	return fn()
}

var (
	envCall    = regexp.MustCompile(`\b(Getenv|LookupEnv|valueOrDefault|envOr|renamed)\(`)
	envLiteral = regexp.MustCompile(`"([A-Z][A-Z0-9_]{2,})"`)
)

// EnvVarsRead scans the Go sources under roots (skipping tests and testdata)
// for environment variables read with os.Getenv, os.LookupEnv or the
// valueOrDefault / envOr / renamed helpers, and returns name → file. Only
// string literals on the same line are found; a name built at run time is
// missed. Roots that do not exist are skipped.
//
// A project's test compares the result with its Catalog (Lookup or
// NotDeployed), so a new variable cannot ship undocumented.
func EnvVarsRead(roots ...string) (map[string]string, error) {
	read := map[string]string{}
	for _, root := range roots {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(content), "\n") {
				if !envCall.MatchString(line) {
					continue
				}
				for _, m := range envLiteral.FindAllStringSubmatch(line, -1) {
					read[m[1]] = path
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return read, nil
}
