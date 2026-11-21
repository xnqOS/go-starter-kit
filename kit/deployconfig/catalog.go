// Package deployconfig is a local web page for writing a project's
// .env.deploy before a deployment: it explains every setting, says which ones
// still need a value, runs the API's own startup validation, generates local
// keys and points at where third-party keys come from.
//
// The settings are data: each project describes its own in a Catalog and
// serves it with Server (see cmd/deploy-config in the starter kit). Pair it
// with a test built on EnvVarsRead so that a feature reading a new
// environment variable has to describe it in the catalog.
package deployconfig

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Values is a .env.deploy as name → value. Missing and empty mean the same.
type Values map[string]string

// Empty reports whether name has no value (blank counts as empty).
func (v Values) Empty(name string) bool { return strings.TrimSpace(v[name]) == "" }

// Level says how much a missing or wrong value matters.
type Level string

const (
	// Required: the API refuses to start, or a feature that is turned on
	// cannot work.
	Required Level = "required"
	// Recommended: the API starts, but production would be wrong or weaker.
	Recommended Level = "recommended"
)

// When returns level and reason if cond holds, otherwise no issue. It keeps
// Var.Need functions short.
func When(cond bool, level Level, reason string) (Level, string) {
	if cond {
		return level, reason
	}
	return "", ""
}

// Link points at the page where a third-party value is created.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Var describes one setting.
type Var struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	Title string `json:"title"`
	// Help explains what the setting does and what to put in it.
	Help string `json:"help"`
	// Default is what the API uses when the setting is empty.
	Default string `json:"default,omitempty"`
	// Suggested is the value a new production .env.deploy starts with.
	Suggested string   `json:"suggested,omitempty"`
	Example   string   `json:"example,omitempty"`
	Options   []string `json:"options,omitempty"`
	Secret    bool     `json:"secret,omitempty"`
	// Generate names a key the page can create locally: "base64-32" (as
	// openssl rand -base64 32) or "hex-32" (as openssl rand -hex 32).
	Generate string `json:"generate,omitempty"`
	// Links and Steps say where a third-party value comes from.
	Links []Link   `json:"links,omitempty"`
	Steps []string `json:"steps,omitempty"`
	// Advanced settings are folded away unless set or needed.
	Advanced bool `json:"advanced,omitempty"`
	// Legacy names an older variable kept working; the page suggests the new
	// name instead.
	Legacy string `json:"legacy,omitempty"`
	// Need says whether, given all values, this one needs attention, and why.
	Need func(Values) (Level, string) `json:"-"`
	// Applies says whether the setting matters with the other choices. The
	// page folds away settings that do not apply and have no value. Nil: always.
	Applies func(Values) bool `json:"-"`
}

// Catalog is a project's list of deployment settings.
type Catalog struct {
	// Title heads the page (default "部署配置").
	Title string
	// Groups orders the sections of the page; every Var.Group must be one.
	Groups []string
	Vars   []Var
	// NotDeployed are variables the code reads that do not belong in
	// .env.deploy (test-only, passed to a one-off command, ...), with the
	// reason. Everything else the code reads should be in Vars.
	NotDeployed map[string]string
	// Startup runs the API's real configuration loading, for example
	// `_, err := config.Load(); return err`. Check calls it with exactly the
	// given values in the process environment (every catalog variable is
	// cleared first and restored afterwards), so it can read os.Getenv. Nil
	// skips the startup check.
	Startup func(Values) error
	// Notes are extra preparation steps outside .env.deploy, shown as a list.
	Notes []string
	// NextSteps is shown after saving, e.g. "在服务器上执行 make deploy".
	NextSteps string
}

// Lookup returns the entry for name.
func (c *Catalog) Lookup(name string) (Var, bool) {
	for _, v := range c.Vars {
		if v.Name == name {
			return v, true
		}
	}
	return Var{}, false
}

// Names lists every variable in the catalog, in order.
func (c *Catalog) Names() []string {
	names := make([]string, len(c.Vars))
	for i, v := range c.Vars {
		names[i] = v.Name
	}
	return names
}

// Validate reports mistakes in the catalog itself: duplicates, missing Title
// or Help, unknown groups or key kinds, a Need that panics on an empty file.
// Call it from a test.
func (c *Catalog) Validate() (err error) {
	var problems []error
	seen := map[string]bool{}
	for _, v := range c.Vars {
		if seen[v.Name] {
			problems = append(problems, fmt.Errorf("%s is listed twice", v.Name))
		}
		seen[v.Name] = true
		if !envName.MatchString(v.Name) {
			problems = append(problems, fmt.Errorf("%q is not a valid variable name", v.Name))
		}
		if v.Title == "" || v.Help == "" {
			problems = append(problems, fmt.Errorf("%s needs a Title and Help", v.Name))
		}
		if !slices.Contains(c.Groups, v.Group) {
			problems = append(problems, fmt.Errorf("%s has unknown group %q", v.Name, v.Group))
		}
		if v.Generate != "" {
			if _, err := Generate(v.Generate); err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", v.Name, err))
			}
		}
		if v.Need != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						problems = append(problems, fmt.Errorf("%s: Need panics on an empty file: %v", v.Name, r))
					}
				}()
				v.Need(Values{})
			}()
		}
	}
	for name := range c.NotDeployed {
		if seen[name] {
			problems = append(problems, fmt.Errorf("%s is both in Vars and NotDeployed", name))
		}
	}
	return errors.Join(problems...)
}

// Generate returns a new random key of the given kind (Var.Generate).
func Generate(kind string) (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	switch kind {
	case "base64-32":
		return base64.StdEncoding.EncodeToString(key), nil
	case "hex-32":
		return hex.EncodeToString(key), nil
	}
	return "", fmt.Errorf("unknown key kind %q", kind)
}

// Issue is one setting that needs attention.
type Issue struct {
	Name   string `json:"name"`
	Level  Level  `json:"level"`
	Reason string `json:"reason"`
}

// Result is the outcome of Check.
type Result struct {
	// StartupChecked is false when the catalog has no Startup function.
	StartupChecked bool `json:"startup_checked"`
	// Startup is the API's own config error, exactly as the deployment would
	// hit it; empty means the API would start.
	Startup string  `json:"startup"`
	Issues  []Issue `json:"issues"`
	// NotApplicable lists settings that do not matter with the current
	// choices (Var.Applies), so the page can fold them away.
	NotApplicable []string `json:"not_applicable"`
}

// Check runs Startup (if any) against values, then every Var.Need.
func (c *Catalog) Check(values Values) Result {
	var result Result
	if c.Startup != nil {
		result.StartupChecked = true
		if err := WithEnv(values, c.Names(), func() error { return c.Startup(values) }); err != nil {
			result.Startup = err.Error()
		}
	}
	for _, v := range c.Vars {
		if v.Applies != nil && !v.Applies(values) {
			result.NotApplicable = append(result.NotApplicable, v.Name)
		}
		if v.Need == nil {
			continue
		}
		if level, reason := v.Need(values); level != "" {
			result.Issues = append(result.Issues, Issue{Name: v.Name, Level: level, Reason: reason})
		}
	}
	return result
}

// Render writes values as a commented .env.deploy grouped like the page.
// Empty catalog values are left out; names not in the catalog are kept at
// the end so nothing the page does not know about is lost.
func (c *Catalog) Render(values Values, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by make deploy-config on %s.\n", now.Format("2006-01-02 15:04"))
	b.WriteString("# Read by compose.deploy.yaml; never commit it. Every setting is explained\n")
	b.WriteString("# on the make deploy-config page.\n")
	for _, group := range c.Groups {
		wrote := false
		for _, v := range c.Vars {
			if v.Group != group || values.Empty(v.Name) {
				continue
			}
			if !wrote {
				fmt.Fprintf(&b, "\n# --- %s ---\n", group)
				wrote = true
			}
			fmt.Fprintf(&b, "# %s\n%s=%s\n", v.Title, v.Name, quote(values[v.Name]))
		}
	}
	var unknown []string
	for name := range values {
		if _, ok := c.Lookup(name); !ok && !values.Empty(name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		b.WriteString("\n# --- Not known to make deploy-config, kept as they were ---\n")
		for _, name := range unknown {
			fmt.Fprintf(&b, "%s=%s\n", name, quote(values[name]))
		}
	}
	return b.String()
}
