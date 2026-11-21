// Package humancheck verifies Cloudflare Turnstile tokens for forms that
// bots should not submit (signup, password reset, contact).
package humancheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

// TurnstileVerifyURL is Cloudflare's siteverify endpoint.
const TurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// ErrRejected means the proof is missing or not valid for this site and
// action; answer it as a client error. Cloudflare outages (unreachable, 5xx,
// unreadable reply or the "internal-error" code) are reported as
// apperror.ErrUnavailable instead, never as "not human".
var ErrRejected = errors.New("human check failed")

// Turnstile checks a token with Cloudflare. Cloudflare enforces freshness
// and single use; Verify also checks that the token was issued on this site
// and for the expected action, so a token from another form or site fails.
type Turnstile struct {
	secret   string
	hostname string
	// allowTestKeys accepts Cloudflare's always-pass testing keys, whose
	// answers carry no real hostname or action. Development only.
	allowTestKeys bool
	// Endpoint and Client can be replaced in tests.
	Endpoint string
	Client   *http.Client
}

// NewTurnstile takes the server-side secret key, the URL of the site where
// the widget runs (its hostname must match), and whether testing keys are
// allowed; pass cfg.Development() for the last one.
func NewTurnstile(secret, siteURL string, allowTestKeys bool) *Turnstile {
	host := ""
	if u, err := url.Parse(siteURL); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	return &Turnstile{secret: secret, hostname: host, allowTestKeys: allowTestKeys, Endpoint: TurnstileVerifyURL, Client: &http.Client{Timeout: 5 * time.Second}}
}

// Verify returns nil for a valid proof, an error wrapping ErrRejected for an
// invalid one, apperror.ErrUnavailable when Cloudflare cannot answer, or
// ctx.Err(). ip is the visitor's address (c.ClientIP()).
func (v *Turnstile) Verify(ctx context.Context, token, ip, action string) error {
	if token == "" || ip == "" {
		return fmt.Errorf("%w: missing proof", ErrRejected)
	}
	form := url.Values{"secret": {v.secret}, "response": {token}, "remoteip": {ip}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := v.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return apperror.ErrUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode >= 500 {
		return apperror.ErrUnavailable
	}
	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
		Hostname   string   `json:"hostname"`
		Action     string   `json:"action"`
		Metadata   struct {
			TestingKey bool `json:"result_with_testing_key"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
		// The body can fail to arrive because the request ended after the
		// headers did; keep that a cancellation, not an outage.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return apperror.ErrUnavailable
	}
	if !out.Success {
		// Cloudflare reports its own failures in a 200 reply; they are
		// retryable, not a verdict on the visitor.
		if slices.Contains(out.ErrorCodes, "internal-error") {
			return apperror.ErrUnavailable
		}
		return ErrRejected
	}
	if out.Metadata.TestingKey {
		if !v.allowTestKeys {
			return fmt.Errorf("%w: testing key used outside development", ErrRejected)
		}
		return nil
	}
	if !strings.EqualFold(out.Hostname, v.hostname) || out.Action != action {
		return fmt.Errorf("%w: proof issued for another site or action", ErrRejected)
	}
	return nil
}
