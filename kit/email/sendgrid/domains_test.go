package sendgrid

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
		if r.Header.Get("Authorization") != "Bearer SG.test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		valid := r.Method == http.MethodGet
		switch {
		case strings.HasSuffix(r.URL.Path, "/validate"):
			fmt.Fprint(w, `{"id":42,"valid":true}`)
			return
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/7"):
			w.WriteHeader(http.StatusNotFound)
			return
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprintf(w, `{"id":42,"domain":"mail.teashop.com","valid":%t,"dns":{"mail_cname":{"valid":%t,"type":"cname","host":"em1.mail.teashop.com","data":"u1.wl.sendgrid.net"},"dkim1":{"valid":%t,"type":"cname","host":"s1._domainkey.mail.teashop.com","data":"s1.domainkey.u1.wl.sendgrid.net"},"dkim2":{"valid":%t,"type":"cname","host":"s2._domainkey.mail.teashop.com","data":"s2.domainkey.u1.wl.sendgrid.net"}}}`, valid, valid, valid, valid)
	}))
	defer server.Close()
	p, err := New(Config{APIKey: "SG.test"})
	if err != nil {
		t.Fatal(err)
	}
	p.WithBaseURL(server.URL)
	var _ email.DomainManager = p
	created, err := p.CreateDomain(t.Context(), "mail.teashop.com")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "42" || created.Status != email.DomainPending || len(created.Records) != 3 || created.Records[0].Purpose != "return_path" || created.Records[1].Purpose != "dkim" || created.Records[0].Type != "CNAME" {
		t.Fatalf("created: %+v", created)
	}
	verified, err := p.VerifyDomain(t.Context(), "42")
	if err != nil || verified.Status != email.DomainVerified || !verified.Records[2].Verified {
		t.Fatalf("verify: %+v %v", verified, err)
	}
	if err := p.DeleteDomain(t.Context(), "42"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteDomain(t.Context(), "7"); err != nil {
		t.Fatalf("deleting an unknown domain: %v", err)
	}
	want := "POST /v3/whitelabel/domains,POST /v3/whitelabel/domains/42/validate,GET /v3/whitelabel/domains/42,DELETE /v3/whitelabel/domains/42,DELETE /v3/whitelabel/domains/7"
	if got := strings.Join(calls, ","); got != want {
		t.Fatalf("calls %s", got)
	}
}

func TestVerifyDomainReportsADeletedDomain(t *testing.T) {
	status := http.StatusNotFound
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	p, err := New(Config{APIKey: "SG.test"})
	if err != nil {
		t.Fatal(err)
	}
	p.WithBaseURL(server.URL)
	if _, err := p.VerifyDomain(t.Context(), "gone"); !errors.Is(err, email.ErrDomainNotFound) {
		t.Fatalf("404: %v", err)
	}
	status = http.StatusServiceUnavailable
	if _, err := p.VerifyDomain(t.Context(), "d_1"); err == nil || errors.Is(err, email.ErrDomainNotFound) {
		t.Fatalf("503 must be a transient error: %v", err)
	}
}
