package deployconfig

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed page.html
var page []byte

// Server serves the page for one .env.deploy file. Every API call needs
// Token (sent as X-Token), so other sites open in the browser cannot read or
// overwrite the file through localhost. Serve it on a loopback address only:
// the page shows and writes secrets.
type Server struct {
	Path    string
	Example string // .env.deploy.example, the starting point for a new file
	Token   string
	Catalog *Catalog
	Now     func() time.Time

	saveMu sync.Mutex
}

type state struct {
	Title     string   `json:"title"`
	Notes     []string `json:"notes"`
	NextSteps string   `json:"next_steps"`
	Path      string   `json:"path"`
	Exists    bool     `json:"exists"`
	Groups    []string `json:"groups"`
	Vars      []Var    `json:"vars"`
	Values    Values   `json:"values"`
	Unknown   []string `json:"unknown"`
	Result    Result   `json:"result"`
	// Problems are lines of the existing file that cannot be read exactly as
	// Compose reads them; while there are any, saving is refused.
	Problems []string `json:"problems"`
}

func (s *Server) Handler() http.Handler {
	if s.Catalog == nil {
		panic("deployconfig: Server.Catalog is nil")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'")
		w.Write(page)
	})
	mux.HandleFunc("GET /api/state", s.auth(s.state))
	mux.HandleFunc("POST /api/check", s.auth(func(w http.ResponseWriter, r *http.Request) {
		values, ok := readValues(w, r)
		if ok {
			writeJSON(w, http.StatusOK, s.Catalog.Check(values))
		}
	}))
	mux.HandleFunc("POST /api/render", s.auth(func(w http.ResponseWriter, r *http.Request) {
		values, ok := readValues(w, r)
		if ok {
			writeJSON(w, http.StatusOK, map[string]string{"content": s.Catalog.Render(values, s.now())})
		}
	}))
	mux.HandleFunc("POST /api/generate", s.auth(func(w http.ResponseWriter, r *http.Request) {
		key, err := Generate(r.URL.Query().Get("kind"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"value": key})
	}))
	mux.HandleFunc("POST /api/save", s.auth(s.save))
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Token")), []byte(s.Token)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "打开终端里打印的完整地址（带 ?t=…）"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	c := s.Catalog
	title := c.Title
	if title == "" {
		title = "部署配置"
	}
	st := state{Title: title, Notes: c.Notes, NextSteps: c.NextSteps, Path: s.Path, Groups: c.Groups, Vars: c.Vars, Values: Values{}}
	content, err := os.ReadFile(s.Path)
	switch {
	case err == nil:
		st.Exists = true
	case errors.Is(err, fs.ErrNotExist):
		// A new file starts from the example, then the production suggestions.
		content, _ = os.ReadFile(s.Example)
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	entries, problems := Parse(string(content))
	if st.Exists {
		st.Problems = problems
	}
	for _, e := range entries {
		st.Values[e.Name] = e.Value
	}
	if !st.Exists {
		for _, v := range c.Vars {
			if v.Suggested != "" && st.Values[v.Name] == "" {
				st.Values[v.Name] = v.Suggested
			}
		}
	}
	for name := range st.Values {
		if _, ok := c.Lookup(name); !ok {
			st.Unknown = append(st.Unknown, name)
		}
	}
	st.Result = c.Check(st.Values)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	values, ok := readValues(w, r)
	if !ok {
		return
	}
	for name, value := range values {
		if !envName.MatchString(name) || strings.ContainsRune(value, 0) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": name + " 的名字或值不能写进 env 文件"})
			return
		}
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	now := s.now()
	backup := ""
	old, err := os.ReadFile(s.Path)
	switch {
	case err == nil:
		// Never rewrite a file whose current meaning we could not read: the
		// new file would silently change values Compose uses today.
		if _, problems := Parse(string(old)); len(problems) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "现有文件里有无法准确读取的行，先手动改好再保存", "problems": problems})
			return
		}
		if backup, err = writeBackup(s.Path, old, now); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "备份失败，没有保存：" + err.Error()})
			return
		}
	case !errors.Is(err, fs.ErrNotExist):
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), "."+filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	_, err = tmp.WriteString(s.Catalog.Render(values, now))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.Path)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": s.Path, "backup": backup, "result": s.Catalog.Check(values)})
}

// writeBackup copies old to a new file next to path. It never overwrites:
// a backup may hold the only copy of an encryption key, so two saves in the
// same second get .bak-<time>, .bak-<time>-2, ...
func writeBackup(path string, old []byte, now time.Time) (string, error) {
	base := path + ".bak-" + now.Format("20060102-150405")
	for n := 1; n < 1000; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(old); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("too many backups with the same timestamp")
}

func readValues(w http.ResponseWriter, r *http.Request) (Values, bool) {
	var values Values
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&values); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不对"})
		return nil, false
	}
	return values, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
