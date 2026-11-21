package resend

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

func TestDomainLifecycle(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer re_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		status := "pending"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/domains":
			status = "not_started"
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/verify"):
			fmt.Fprint(w, `{"object":"domain","id":"d_1"}`)
			return
		case r.Method == http.MethodGet:
			status = "verified"
		case r.Method == http.MethodDelete && r.URL.Path == "/domains/gone":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"name":"not_found"}`)
			return
		case r.Method == http.MethodDelete:
			fmt.Fprint(w, `{"object":"domain","id":"d_1","deleted":true}`)
			return
		}
		fmt.Fprintf(w, `{"id":"d_1","name":"mail.teashop.com","status":%q,"records":[{"record":"DKIM","name":"resend._domainkey","type":"TXT","value":"p=abc","status":%q},{"record":"SPF","name":"send","type":"MX","value":"feedback-smtp.example.com","priority":10,"status":"pending"}]}`, status, status)
	}))
	defer server.Close()
	p := New(Config{APIKey: "re_test"}).WithBaseURL(server.URL)
	var _ email.DomainManager = p
	created, err := p.CreateDomain(t.Context(), "mail.teashop.com")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "d_1" || created.Status != email.DomainPending || len(created.Records) != 2 {
		t.Fatalf("created: %+v", created)
	}
	if r := created.Records[0]; r.Purpose != "dkim" || r.Name != "resend._domainkey.mail.teashop.com" || r.Type != "TXT" {
		t.Fatalf("dkim record: %+v", r)
	}
	if r := created.Records[1]; r.Type != "MX" || r.Priority != 10 || r.Name != "send.mail.teashop.com" {
		t.Fatalf("mx record: %+v", r)
	}
	verified, err := p.VerifyDomain(t.Context(), "d_1")
	if err != nil || verified.Status != email.DomainVerified || !verified.Records[0].Verified {
		t.Fatalf("verify: %+v %v", verified, err)
	}
	if err := p.DeleteDomain(t.Context(), "d_1"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteDomain(t.Context(), "gone"); err != nil {
		t.Fatalf("deleting an unknown domain: %v", err)
	}
	want := "POST /domains,POST /domains/d_1/verify,GET /domains/d_1,DELETE /domains/d_1,DELETE /domains/gone"
	if got := strings.Join(calls, ","); got != want {
		t.Fatalf("calls %s", got)
	}
	if _, err := New(Config{APIKey: "wrong"}).WithBaseURL(server.URL).CreateDomain(t.Context(), "x.com"); email.Classify(err) != email.Permanent {
		t.Fatalf("bad key: %v", err)
	}
}

func TestVerifyDomainReportsADeletedDomain(t *testing.T) {
	status := http.StatusNotFound
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	p := New(Config{APIKey: "re_test"}).WithBaseURL(server.URL)
	if _, err := p.VerifyDomain(t.Context(), "gone"); !errors.Is(err, email.ErrDomainNotFound) {
		t.Fatalf("404: %v", err)
	}
	status = http.StatusServiceUnavailable
	if _, err := p.VerifyDomain(t.Context(), "d_1"); err == nil || errors.Is(err, email.ErrDomainNotFound) {
		t.Fatalf("503 must be a transient error: %v", err)
	}
}
