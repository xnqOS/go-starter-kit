package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestKeys(t *testing.T) {
	for _, ok := range []string{"IMG.png", "T1/S1/IMG.jpg", "a_b-c/D-e_f/G.pdf", "a/b/c/d/e/f/g/h.bin"} {
		if !ValidKey(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "IMG", "../IMG.png", "/T1/IMG.png", "T1//IMG.png", "T1/../IMG.png", "T1/IMG.PNG",
		"a.b/IMG.png", "IMG.tar.gz", "T1/IMG.png/", "a/b/c/d/e/f/g/h/i.bin", "sp ace.png", `back\slash.png`} {
		if ValidKey(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	key, err := Key("png", "T1", "S1", "IMG")
	if err != nil || key != "T1/S1/IMG.png" {
		t.Fatalf("Key = %q, %v", key, err)
	}
	for _, parts := range [][]string{nil, {"legacy tenant", "IMG"}, {"..", "IMG"}, {"a", "b", "c", "d", "e", "f", "g", "h", "i"}} {
		if _, err := Key("png", parts...); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Key(%q) = %v", parts, err)
		}
	}
	if _, err := Key("PNG", "IMG"); !errors.Is(err, ErrInvalidKey) {
		t.Error("uppercase extension accepted")
	}
	if got := ContentType("a/b.png"); got != "image/png" {
		t.Errorf("png: %q", got)
	}
	if got := ContentType("a/b.zzzunknown"); got != "application/octet-stream" {
		t.Errorf("unknown: %q", got)
	}
	if !errors.Is(ErrNotFound, os.ErrNotExist) || !errors.Is(ErrExists, os.ErrExist) {
		t.Fatal("sentinel errors must match the os errors")
	}
}

func TestLocalPrunesEmptyFolders(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocal(dir)
	must(t, err)
	must(t, s.Put(ctx, "T1/S1/A.png", []byte("a")))
	must(t, s.Put(ctx, "T1/S1/B.png", []byte("b")))
	must(t, s.Put(ctx, "T1/S2/C.png", []byte("c")))
	if _, err := os.Stat(dir + "/T1/S1/A.png"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "T1/S1/A.png", []byte("again")); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite: %v", err)
	}
	f, err := s.Open(ctx, "T1/S1/A.png")
	must(t, err)
	data, _ := io.ReadAll(f)
	must(t, f.Close())
	if string(data) != "a" {
		t.Fatalf("read %q", data)
	}
	must(t, s.Delete(ctx, "T1/S1/A.png"))
	if _, err := os.Stat(dir + "/T1/S1"); err != nil {
		t.Fatal("folder with a remaining object was removed")
	}
	must(t, s.Delete(ctx, "T1/S1/B.png"))
	if _, err := os.Stat(dir + "/T1/S1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty folder kept")
	}
	if _, err := os.Stat(dir + "/T1"); err != nil {
		t.Fatal("parent folder with other objects was removed")
	}
	must(t, s.Delete(ctx, "T1/S2/C.png"))
	must(t, s.Delete(ctx, "T1/S2/C.png"))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %d entries", len(entries))
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("storage root removed")
	}
	if _, err := s.Open(ctx, "T1/S2/C.png"); !errors.Is(err, ErrNotFound) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing object: %v", err)
	}
}

func TestLocalEscapeProtection(t *testing.T) {
	if _, err := NewLocal(""); err == nil {
		t.Fatal("storage should require an explicit directory")
	}
	directory := t.TempDir()
	s, err := NewLocal(directory)
	must(t, err)
	if err = s.Put(ctx, "../escape.png", []byte("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("path traversal: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	must(t, os.WriteFile(outside, []byte("untouched"), 0600))
	must(t, os.Symlink(outside, filepath.Join(directory, "escape.png")))
	if _, err = s.Open(ctx, "escape.png"); err == nil {
		t.Fatal("symlink escaped storage root")
	}
	if err = s.Put(ctx, "escape.png", []byte("replace")); err == nil {
		t.Fatal("existing file overwritten")
	}
	data, err := os.ReadFile(outside)
	must(t, err)
	if string(data) != "untouched" {
		t.Fatal("outside file changed")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Put(canceled, "A.png", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled put: %v", err)
	}
}

const workerSecret = "test-secret-0123456789-0123456789-abcd"

// fakeWorker implements the Worker protocol documented on Worker.
func fakeWorker(t *testing.T) (*httptest.Server, map[string][]byte) {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+workerSecret {
			http.Error(w, `{"code":"unauthorized"}`, 401)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/objects/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("Content-Type") != ContentType(key) || r.ContentLength <= 0 {
				http.Error(w, "bad request", 400)
				return
			}
			if _, ok := objects[key]; ok {
				http.Error(w, `{"code":"conflict"}`, 409)
				return
			}
			data, _ := io.ReadAll(r.Body)
			objects[key] = data
			w.WriteHeader(201)
		case http.MethodGet:
			data, ok := objects[key]
			if !ok {
				http.Error(w, "missing", 404)
				return
			}
			_, _ = w.Write(data)
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, objects
}

func TestWorkerContract(t *testing.T) {
	srv, objects := fakeWorker(t)
	s, err := NewWorker(srv.URL+"/", workerSecret, WorkerOptions{Client: srv.Client(), MaxSize: 16})
	must(t, err)
	must(t, s.Put(ctx, "T1/A1.png", []byte("png-bytes")))
	if err := s.Put(ctx, "T1/A1.png", []byte("again")); !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "409") {
		t.Fatalf("overwrite allowed: %v", err)
	}
	if err := s.Put(ctx, "T1/big.png", make([]byte, 17)); err == nil {
		t.Fatal("object over MaxSize accepted")
	}
	f, err := s.Open(ctx, "T1/A1.png")
	must(t, err)
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(f)
	if string(rest) != "bytes" {
		t.Fatalf("read %q", rest)
	}
	must(t, s.Delete(ctx, "T1/A1.png"))
	must(t, s.Delete(ctx, "T1/A1.png"))
	if _, err := s.Open(ctx, "T1/A1.png"); !errors.Is(err, ErrNotFound) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing object: %v", err)
	}
	if err := s.Put(ctx, "../x.png", []byte("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("path traversal key: %v", err)
	}
	if len(objects) != 0 {
		t.Fatal("objects left behind")
	}
	objects["huge.bin"] = make([]byte, 17)
	if _, err := s.Open(ctx, "huge.bin"); err == nil {
		t.Fatal("oversized object read")
	}
	wrong, err := NewWorker(srv.URL, strings.Repeat("w", 40), WorkerOptions{Client: srv.Client()})
	must(t, err)
	if err := wrong.Put(ctx, "B.png", []byte("x")); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong secret: %v", err)
	}
}

func TestWorkerConfigValidation(t *testing.T) {
	for _, tc := range []struct{ url, secret string }{
		{"http://files.example.com", workerSecret},
		{"https://user:pw@files.example.com", workerSecret},
		{"https://files.example.com?x=1", workerSecret},
		{"https://files.example.com#x", workerSecret},
		{"files.example.com", workerSecret},
		{"https://files.example.com", "short"},
	} {
		if _, err := NewWorker(tc.url, tc.secret, WorkerOptions{}); err == nil {
			t.Fatalf("accepted %q", tc.url)
		}
	}
	for _, ok := range []string{"https://files.example.workers.dev", "http://localhost:8787", "http://127.0.0.1:8787", "http://[::1]:8787"} {
		if _, err := NewWorker(ok, workerSecret, WorkerOptions{}); err != nil {
			t.Fatalf("rejected %q: %v", ok, err)
		}
	}
}

// Both implementations satisfy the interface.
var (
	_ Storage = (*Local)(nil)
	_ Storage = (*Worker)(nil)
)
