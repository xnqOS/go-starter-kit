package deployconfig

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testCatalog is a small project catalog: a mail provider whose SMTP server
// is required only with MAIL_PROVIDER=smtp, and a generated key.
func testCatalog() *Catalog {
	return &Catalog{
		Title:  "Test 部署配置",
		Groups: []string{"基础", "邮件"},
		Vars: []Var{
			{Name: "APP_ENV", Group: "基础", Title: "环境", Help: "production 或 staging。", Suggested: "production"},
			{Name: "APP_KEY", Group: "基础", Title: "加密密钥", Help: "生成一次，不要更换。", Secret: true, Generate: "base64-32",
				Need: func(v Values) (Level, string) { return When(v.Empty("APP_KEY"), Recommended, "点「生成」") }},
			{Name: "MAIL_PROVIDER", Group: "邮件", Title: "发件服务", Help: "smtp 或 disabled。", Options: []string{"disabled", "smtp"}, Suggested: "smtp"},
			{Name: "SMTP_ADDRESS", Group: "邮件", Title: "SMTP 服务器", Help: "host:port。",
				Links:   []Link{{Label: "Provider", URL: "https://example.com"}},
				Applies: func(v Values) bool { return v["MAIL_PROVIDER"] == "smtp" },
				Need: func(v Values) (Level, string) {
					return When(v["MAIL_PROVIDER"] == "smtp" && v.Empty("SMTP_ADDRESS"), Required, "MAIL_PROVIDER=smtp 时必填")
				}},
		},
		NotDeployed: map[string]string{"TEST_ONLY": "只给测试用"},
		Startup: func(Values) error {
			if os.Getenv("MAIL_PROVIDER") == "smtp" && os.Getenv("SMTP_ADDRESS") == "" {
				return errors.New("MAIL_PROVIDER=smtp requires SMTP_ADDRESS")
			}
			return nil
		},
		Notes:     []string{"配置反向代理。"},
		NextSteps: "在服务器上执行 make deploy。",
	}
}

func production() Values {
	key, _ := Generate("base64-32")
	return Values{"APP_ENV": "production", "APP_KEY": key, "MAIL_PROVIDER": "smtp", "SMTP_ADDRESS": "smtp.example.com:587"}
}

func hasIssue(r Result, name string, level Level) bool {
	for _, i := range r.Issues {
		if i.Name == name && i.Level == level {
			return true
		}
	}
	return false
}

func TestCheck(t *testing.T) {
	c := testCatalog()
	t.Setenv("SMTP_ADDRESS", "leaked:25") // the caller's environment must not leak in
	if got := c.Check(production()); got.Startup != "" || len(got.Issues) != 0 || !got.StartupChecked {
		t.Fatalf("complete config: %+v", got)
	}
	values := production()
	delete(values, "SMTP_ADDRESS")
	got := c.Check(values)
	if !strings.Contains(got.Startup, "SMTP_ADDRESS") || !hasIssue(got, "SMTP_ADDRESS", Required) {
		t.Errorf("missing SMTP server: %+v", got)
	}
	if os.Getenv("SMTP_ADDRESS") != "leaked:25" {
		t.Fatal("Check did not restore the environment")
	}
	values = production()
	delete(values, "APP_KEY")
	if got := c.Check(values); got.Startup != "" || !hasIssue(got, "APP_KEY", Recommended) {
		t.Errorf("without key: %+v", got)
	}
	values = production()
	values["MAIL_PROVIDER"] = "disabled"
	if got := c.Check(values); !slices.Contains(got.NotApplicable, "SMTP_ADDRESS") || slices.Contains(got.NotApplicable, "APP_KEY") {
		t.Errorf("not applicable: %v", got.NotApplicable)
	}
	c.Startup = nil
	if got := c.Check(Values{"MAIL_PROVIDER": "smtp"}); got.StartupChecked || got.Startup != "" {
		t.Errorf("without Startup: %+v", got)
	}
}

func TestWithEnv(t *testing.T) {
	t.Setenv("DEPLOYCONFIG_A", "old")
	os.Unsetenv("DEPLOYCONFIG_B")
	err := WithEnv(Values{"DEPLOYCONFIG_B": "new", "DEPLOYCONFIG_C": ""}, []string{"DEPLOYCONFIG_A"}, func() error {
		if _, ok := os.LookupEnv("DEPLOYCONFIG_A"); ok {
			t.Error("listed name was not cleared")
		}
		if os.Getenv("DEPLOYCONFIG_B") != "new" {
			t.Error("value not set")
		}
		if _, ok := os.LookupEnv("DEPLOYCONFIG_C"); ok {
			t.Error("empty value should stay unset")
		}
		return errors.New("boom")
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
	if os.Getenv("DEPLOYCONFIG_A") != "old" {
		t.Error("previous value not restored")
	}
	if _, ok := os.LookupEnv("DEPLOYCONFIG_B"); ok {
		t.Error("temporary value left behind")
	}
}

func TestValidate(t *testing.T) {
	if err := testCatalog().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := testCatalog()
	bad.Vars = append(bad.Vars,
		Var{Name: "APP_ENV", Group: "基础", Title: "x", Help: "x"},
		Var{Name: "NO_HELP", Group: "基础", Title: "x"},
		Var{Name: "BAD_GROUP", Group: "nope", Title: "x", Help: "x"},
		Var{Name: "BAD_KEY", Group: "基础", Title: "x", Help: "x", Generate: "rot13"},
		Var{Name: "PANICS", Group: "基础", Title: "x", Help: "x", Need: func(v Values) (Level, string) { panic("nil map") }},
		Var{Name: "TEST_ONLY", Group: "基础", Title: "x", Help: "x"},
	)
	err := bad.Validate()
	for _, want := range []string{"APP_ENV is listed twice", "NO_HELP needs", "BAD_GROUP has unknown group", "BAD_KEY", "PANICS: Need panics", "TEST_ONLY is both"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %v, want %q", err, want)
		}
	}
}

func TestGenerate(t *testing.T) {
	key, err := Generate("base64-32")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := base64.StdEncoding.DecodeString(key); len(raw) != 32 {
		t.Errorf("want 32 bytes, got %d", len(raw))
	}
	if hex, _ := Generate("hex-32"); len(hex) != 64 {
		t.Errorf("hex-32 length %d", len(hex))
	}
	if other, _ := Generate("base64-32"); other == key {
		t.Error("two generated keys are equal")
	}
	if _, err := Generate("rot13"); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestEnvVarsRead(t *testing.T) {
	dir := t.TempDir()
	write := func(path, body string) {
		full := filepath.Join(dir, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o600)
	}
	write("config/config.go", "package config\nvar a = os.Getenv(\"APP_ENV\")\nvar b = valueOrDefault(\"HTTP_ADDR\", \":8080\")\nvar c = \"NOT_READ_HERE\"\n")
	write("config/config_test.go", "package config\nvar d = os.Getenv(\"TEST_ONLY\")\n")
	write("config/testdata/x.go", "package x\nvar e = os.Getenv(\"FIXTURE\")\n")
	write("cmd/tool/main.go", "package main\nfunc f() { _, _ = os.LookupEnv(\"TOOL_TOKEN\") }\n")
	got, err := EnvVarsRead(filepath.Join(dir, "config"), filepath.Join(dir, "cmd"), filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"APP_ENV", "HTTP_ADDR", "TOOL_TOKEN"}
	var names []string
	for name := range got {
		names = append(names, name)
	}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("EnvVarsRead = %v, want %v", names, want)
	}
}

// trickyValues are values whose quoting matters to Compose.
var trickyValues = Values{
	"A_HASH":       "Shop #1 <a@b.com>",
	"A_APOS_DOLL":  "it's $DEPLOY_CONFIG_REVIEW_UNSET",
	"A_DOLLARS":    "pa$$word",
	"A_ALL":        `it's "$q" \n \ ${X} #`,
	"A_BRACE":      "${NOT_A_VAR}",
	"A_MULTILINE":  "first\nsecond",
	"A_MULTI_APOS": "it's\n$X",
	"A_SPACES":     "  padded  ",
	"A_BASE64":     "rah9GX03ag0s68frFz9rFYzhDn3hn900s87kJlc3Zlc=",
	"A_TRAIL_BS":   `foo\`,
	"A_MID_BS":     `a\b\\c`,
	"A_TAB_HASH":   "foo\t#bar",
	"A_CRLF":       "x\r\ny",
	"A_CR":         "x\ry",
}

func parseValues(t *testing.T, content string) Values {
	t.Helper()
	entries, problems := Parse(content)
	if len(problems) > 0 {
		t.Fatalf("problems: %v\n%s", problems, content)
	}
	got := Values{}
	for _, e := range entries {
		got[e.Name] = e.Value
	}
	return got
}

func TestParseAndRenderRoundTrip(t *testing.T) {
	values := production()
	for k, v := range trickyValues {
		values[k] = v
	}
	content := testCatalog().Render(values, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	got := parseValues(t, content)
	for name, want := range values {
		if got[name] != want {
			t.Errorf("%s: got %q, want %q", name, got[name], want)
		}
	}
	if !strings.Contains(content, "# --- 邮件 ---") || !strings.Contains(content, "# 发件服务\nMAIL_PROVIDER=smtp") || !strings.Contains(content, "A_BASE64=rah9") {
		t.Errorf("rendered:\n%s", content)
	}
}

func TestParseMatchesCompose(t *testing.T) {
	// Each case was checked against `docker compose config`.
	content := "# c\r\nexport A=1\nB = 'x # y'\nC=plain # comment\nD=\"q\\\"d \\$X \\\\ $$ \\n.\"\n" +
		"E=pa$$word\nF='first\nsecond'\nG=\"multi\nline\"\nH=a#b\nI=\nJ='$X \\n'\n"
	got := parseValues(t, content)
	want := Values{"A": "1", "B": "x # y", "C": "plain", "D": "q\"d $X \\ $ \n.", "E": "pa$word",
		"F": "first\nsecond", "G": "multi\nline", "H": "a#b", "I": "", "J": `$X \n`}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: got %q, want %q", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}

	// Things whose meaning depends on the shell running make deploy, or that
	// are malformed, are reported instead of guessed.
	for _, bad := range []string{
		"P=pa$word\n", "P=${X}\n", "P=\"it's $X\"\n", "P=\"\\q\"\n",
		"P='unterminated\nQ=1\n", "P=\"unterminated\n", "JUST_A_NAME\n", "1BAD=x\n", "P='a' trailing\n",
		"P='foo\\'\n", "P='a\\'b'\n",
	} {
		if _, problems := Parse(bad); len(problems) == 0 {
			t.Errorf("Parse(%q) reported no problem", bad)
		}
	}
	if _, problems := Parse("A=1\nB=$X\n"); len(problems) != 1 || !strings.HasPrefix(problems[0], "第 2 行") {
		t.Errorf("line numbers: %v", problems)
	}
}

// composeRead returns the environment real Docker Compose reads from an
// env_file with content. Skips the test where Docker Compose is missing.
func composeRead(t *testing.T, content string) Values {
	t.Helper()
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose not available")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  x:\n    image: busybox\n    env_file: [e.env]\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "e.env"), []byte(content), 0o600)
	cmd := exec.Command("docker", "compose", "config", "--format", "json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME=deployconfigtest")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose config: %v\n%s\nfile:\n%q", err, out, content)
	}
	var cfg struct {
		Services map[string]struct{ Environment map[string]*string }
	}
	if err := json.Unmarshal(out[strings.IndexByte(string(out), '{'):], &cfg); err != nil {
		t.Fatal(err)
	}
	got := Values{}
	for name, value := range cfg.Services["x"].Environment {
		if value != nil {
			// compose config writes a literal "$" as "$$" in its output.
			got[name] = strings.ReplaceAll(*value, "$$", "$")
		}
	}
	return got
}

func sameValues(t *testing.T, what string, got, want Values) {
	t.Helper()
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: %s = %q, want %q", what, name, got[name], w)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: unexpected %s = %q", what, name, got[name])
		}
	}
}

// TestRenderWithCompose writes tricky values and reads them back with the
// real `docker compose config`.
func TestRenderWithCompose(t *testing.T) {
	sameValues(t, "Compose reading Render", composeRead(t, testCatalog().Render(trickyValues, time.Now())), trickyValues)
}

// TestExistingFileWithCompose: what Parse reads from an existing file must be
// what Compose reads, and rendering it again must not change what Compose reads.
func TestExistingFileWithCompose(t *testing.T) {
	existing := "# comment\r\n" +
		"export A=1\r\n" +
		"TAB_HASH=foo\t#bar\n" +
		"SPACE_HASH=foo #bar\n" +
		"TAB_SPACE_HASH=foo\t #bar\n" +
		"TRAILING=foo \t\n" +
		"DOLLARS=pa$$word\n" +
		"BARE_CR=x\ry\n" +
		"SINGLE_CRLF='x\r\ny'\r\n" +
		"DOUBLE_CRLF=\"x\r\ny\"\r\n" +
		"SINGLE_BS='a\\\\b\\x'\n" +
		"DOUBLE='q\"' # c\n" +
		"DOUBLE_ESC=\"\\t\\r\\n\\\\ \\\" \\$X $$\"\n" +
		"MULTI='first\nsecond'\n" +
		"  SPACED  =  v  \n" +
		"EMPTY=\n"
	want := composeRead(t, existing)
	got := parseValues(t, existing)
	sameValues(t, "Parse of existing file", got, want)
	sameValues(t, "Compose reading Render(Parse)", composeRead(t, testCatalog().Render(got, time.Now())), withoutEmpty(want))
}

func withoutEmpty(v Values) Values {
	out := Values{}
	for name, value := range v {
		if value != "" {
			out[name] = value // Render leaves empty settings out
		}
	}
	return out
}

func TestServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.deploy")
	example := filepath.Join(dir, ".env.deploy.example")
	os.WriteFile(example, []byte("APP_ENV=production\n"), 0o600)
	srv := &Server{Path: path, Example: example, Token: "tok", Catalog: testCatalog(),
		Now: func() time.Time { return time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC) }}
	h := srv.Handler()
	do := func(method, target, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if token != "" {
			req.Header.Set("X-Token", token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("GET", "/", "", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "部署配置") {
		t.Fatalf("page: %d", rec.Code)
	}
	if rec := do("GET", "/api/state", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("state without token: %d", rec.Code)
	}
	if rec := do("POST", "/api/save", "wrong", "{}"); rec.Code != http.StatusForbidden {
		t.Fatalf("save with wrong token: %d", rec.Code)
	}

	rec := do("GET", "/api/state", "tok", "")
	var st state
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("state: %d %s", rec.Code, rec.Body)
	}
	if st.Exists || st.Values["APP_ENV"] != "production" || st.Values["MAIL_PROVIDER"] != "smtp" {
		t.Errorf("new file should start from the example plus suggestions: %+v", st.Values)
	}
	if st.Title != "Test 部署配置" || len(st.Notes) != 1 || st.NextSteps == "" {
		t.Errorf("catalog texts: %+v", st)
	}
	if !hasIssue(st.Result, "SMTP_ADDRESS", Required) || !st.Result.StartupChecked {
		t.Errorf("new file should ask for the SMTP server: %+v", st.Result)
	}
	for _, v := range st.Vars {
		if v.Name == "SMTP_ADDRESS" && len(v.Links) == 0 {
			t.Error("vars lost their links in JSON")
		}
	}

	if rec := do("POST", "/api/generate?kind=base64-32", "tok", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"value"`) {
		t.Errorf("generate: %d %s", rec.Code, rec.Body)
	}

	body, _ := json.Marshal(production())
	if rec := do("POST", "/api/save", "tok", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v %v", info, err)
	}
	rec = do("POST", "/api/save", "tok", string(body))
	var saved struct{ Backup string }
	json.Unmarshal(rec.Body.Bytes(), &saved)
	if saved.Backup == "" {
		t.Error("second save should back up the first")
	} else if _, err := os.Stat(saved.Backup); err != nil {
		t.Error(err)
	}
	rec = do("GET", "/api/state", "tok", "")
	json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Exists || st.Result.Startup != "" || len(st.Result.Issues) != 0 {
		t.Errorf("reloaded saved file: %+v", st.Result)
	}
}

func TestSaveKeepsEveryBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.deploy")
	os.WriteFile(path, []byte("ORIGINAL=1\n"), 0o600)
	srv := &Server{Path: path, Token: "tok", Catalog: testCatalog(), Now: func() time.Time { return time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC) }}
	h := srv.Handler()
	var backups []string
	for i := range 3 { // all within the same (frozen) second, like a double-click
		body, _ := json.Marshal(Values{"SAVE": strconv.Itoa(i)})
		req := httptest.NewRequest("POST", "/api/save", strings.NewReader(string(body)))
		req.Header.Set("X-Token", "tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var res struct{ Backup string }
		json.Unmarshal(rec.Body.Bytes(), &res)
		if rec.Code != http.StatusOK || res.Backup == "" {
			t.Fatalf("save %d: %d %s", i, rec.Code, rec.Body)
		}
		backups = append(backups, res.Backup)
	}
	if backups[0] == backups[1] || backups[1] == backups[2] {
		t.Fatalf("backups reused a name: %v", backups)
	}
	if first, _ := os.ReadFile(backups[0]); string(first) != "ORIGINAL=1\n" {
		t.Errorf("first backup was overwritten: %q", first)
	}
	if third, _ := os.ReadFile(backups[2]); !strings.Contains(string(third), "SAVE=1") {
		t.Errorf("third backup: %q", third)
	}
}

func TestSaveRefusesFileItCannotReadExactly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.deploy")
	original := "SMTP_PASSWORD='first\nsecond\nQ=$HOME\n"
	os.WriteFile(path, []byte(original), 0o600)
	srv := &Server{Path: path, Token: "tok", Catalog: testCatalog()}
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Header.Set("X-Token", "tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var st state
	json.Unmarshal(rec.Body.Bytes(), &st)
	if len(st.Problems) == 0 {
		t.Errorf("state should list problems: %s", rec.Body)
	}

	req = httptest.NewRequest("POST", "/api/save", strings.NewReader(`{"SMTP_ADDRESS":"x"}`))
	req.Header.Set("X-Token", "tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("save: %d %s", rec.Code, rec.Body)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Errorf("file was rewritten: %q", got)
	}
	if matches, _ := filepath.Glob(path + ".*"); len(matches) != 0 {
		t.Errorf("left files behind: %v", matches)
	}
}
