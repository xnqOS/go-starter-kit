package deploycatalog

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/xnqOS/go-starter-kit/kit/deployconfig"
)

// TestCatalogCoversEveryEnvironmentVariable is the pre-deployment rule: code
// that reads a new environment variable must describe it in New (Title, Help,
// Need) or list it in NotDeployed, and add it to .env.deploy.example.
func TestCatalogCoversEveryEnvironmentVariable(t *testing.T) {
	c := New()
	// ../../kit exists only in the starter kit itself; a project made with
	// make init gets those packages as a dependency and keeps their
	// variables listed here by hand.
	read, err := deployconfig.EnvVarsRead("../../internal", "../../cmd", "../../kit")
	if err != nil {
		t.Fatal(err)
	}
	if len(read) < 3 {
		t.Fatalf("found only %d environment variables; the scan is probably broken", len(read))
	}
	for name, path := range read {
		if _, ok := c.Lookup(name); ok {
			continue
		}
		if _, ok := c.NotDeployed[name]; ok {
			continue
		}
		t.Errorf("%s reads %s, which internal/deploycatalog does not know.\n"+
			"Add it to New in internal/deploycatalog/catalog.go (Title, Help, Need; Links/Steps for third-party values, Generate for local keys),\n"+
			"or to NotDeployed with the reason it is not a deployment setting. Then document it in .env.deploy.example.", path, name)
	}

	example, err := os.ReadFile("../../.env.deploy.example")
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]bool{}
	for _, v := range c.Vars {
		legacy[v.Legacy] = true
	}
	for _, v := range c.Vars {
		if legacy[v.Name] {
			continue // an older name; the example only shows the current one
		}
		if !regexp.MustCompile(`(?m)^#? ?` + v.Name + `=`).Match(example) {
			t.Errorf("%s is in the catalog but not in .env.deploy.example", v.Name)
		}
	}
}

func TestCatalogIsWellFormed(t *testing.T) {
	if err := New().Validate(); err != nil {
		t.Fatal(err)
	}
}

// The example file, with a project name, is a working production config.
func TestExampleStartsCleanly(t *testing.T) {
	content, err := os.ReadFile("../../.env.deploy.example")
	if err != nil {
		t.Fatal(err)
	}
	entries, problems := deployconfig.Parse(string(content))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	values := deployconfig.Values{}
	for _, e := range entries {
		values[e.Name] = e.Value
	}
	values["LOG_SERVICE"] = "order-api"
	got := New().Check(values)
	if !got.StartupChecked || got.Startup != "" || len(got.Issues) != 0 {
		t.Fatalf("example config: %+v", got)
	}

	values["STORAGE"] = "postgres"
	got = New().Check(values)
	if len(got.Issues) == 0 {
		t.Fatalf("postgres with a file path should need attention: %+v", got)
	}
	values["LOG_OUTPUTS"], values["LOG_SYSLOG_NETWORK"] = "syslog", "nope"
	if got := New().Check(values); !strings.Contains(got.Startup, "LOG_SYSLOG_NETWORK") {
		t.Fatalf("the real config validation should run: %+v", got)
	}
}
