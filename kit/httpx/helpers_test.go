package httpx

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

func TestResultAndPageResultMapErrors(t *testing.T) {
	for _, renderError := range []func(*gin.Context, error){
		func(c *gin.Context, err error) { Result(c, 200, nil, err) },
		func(c *gin.Context, err error) { PageResult(c, []string{}, Pagination{}, err) },
		Fail,
	} {
		w, _ := render(t, func(c *gin.Context) { renderError(c, fmt.Errorf("wrapped: %w", apperror.ErrUnavailable)) })
		if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("missing retry advice: %d %v", w.Code, w.Header())
		}
	}
	w, body := render(t, func(c *gin.Context) { Result(c, 201, map[string]string{"id": "1"}, nil) })
	if w.Code != 201 || string(body["code"]) != `"ok"` {
		t.Fatalf("success: %d %s", w.Code, body["code"])
	}
}

func TestBearerToken(t *testing.T) {
	for _, tc := range []struct{ header, want string }{{"Bearer abc", "abc"}, {"bEaReR\t abc ", "abc"}, {"Bearer abc extra", ""}, {"Basic abc", ""}, {"Bearer", ""}, {"", ""}} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/", nil)
		c.Request.Header.Set("Authorization", tc.header)
		if got := BearerToken(c); got != tc.want {
			t.Fatalf("%q: %q", tc.header, got)
		}
	}
}

func TestParseListParams(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  ListParams
		ok    bool
	}{
		{"", ListParams{}, true},
		{"?limit=0&offset=40", ListParams{Offset: 40}, true},
		{"?limit=-1", ListParams{Limit: -1}, true}, // bounds belong to the service
		{"?limit=abc", ListParams{}, false},
		{"?limit=5&offset=1.5", ListParams{Limit: 5}, false},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/"+tc.query, nil)
		got, ok := ParseListParams(c)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("%q: %+v %v", tc.query, got, ok)
		}
		if !ok && w.Code != 400 {
			t.Fatalf("%q: status %d", tc.query, w.Code)
		}
	}
}

func TestBindJSONCallsOnlyOnValidBody(t *testing.T) {
	called := false
	w, _ := renderBody(t, `{"name":"a"}`, func(c *gin.Context) {
		BindJSON(c, func(in struct{ Name string }) { called = in.Name == "a"; NoContent(c) })
	})
	if !called || w.Code != 204 {
		t.Fatalf("valid body: called=%v status=%d", called, w.Code)
	}
	called = false
	w, _ = renderBody(t, `{"unknown":1}`, func(c *gin.Context) {
		BindJSON(c, func(struct{ Name string }) { called = true })
	})
	if called || w.Code != 400 {
		t.Fatalf("invalid body: called=%v status=%d", called, w.Code)
	}
}

func renderBody(t *testing.T, body string, handler gin.HandlerFunc) (*httptest.ResponseRecorder, string) {
	t.Helper()
	router := gin.New()
	router.POST("/", handler)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w, w.Body.String()
}
