package resend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

type domainReply struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Records []struct {
		Record   string `json:"record"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		Value    string `json:"value"`
		Status   string `json:"status"`
		Priority int    `json:"priority"`
	} `json:"records"`
}

func (p *Provider) domainCall(ctx context.Context, method, path string, body any) (email.HTTPResponse, error) {
	header := http.Header{"Authorization": {"Bearer " + p.config.APIKey}}
	resp, err := email.DoJSON(ctx, p.client, method, p.baseURL+path, header, body)
	if err != nil {
		return resp, err
	}
	if resp.Status/100 != 2 {
		var failure struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(resp.Body, &failure)
		return resp, email.StatusError(email.ClassifyStatus(resp.Status), resp.Status, failure.Name)
	}
	return resp, nil
}

func (p *Provider) domain(resp email.HTTPResponse) (email.Domain, error) {
	var reply domainReply
	if err := json.Unmarshal(resp.Body, &reply); err != nil || reply.ID == "" {
		return email.Domain{}, email.Failed(email.Permanent, "decode", err)
	}
	out := email.Domain{ID: reply.ID, Status: email.DomainPending}
	switch reply.Status {
	case "verified":
		out.Status = email.DomainVerified
	case "failed":
		out.Status = email.DomainFailed
	}
	for _, r := range reply.Records {
		name := r.Name
		// Resend gives names relative to the domain ("resend._domainkey").
		if reply.Name != "" && name != reply.Name && !strings.HasSuffix(name, "."+reply.Name) {
			name += "." + reply.Name
		}
		out.Records = append(out.Records, email.DNSRecord{
			Purpose: strings.ToLower(r.Record), Type: strings.ToUpper(r.Type), Name: name, Value: r.Value,
			Priority: r.Priority, Verified: r.Status == "verified",
		})
	}
	return out, nil
}

// CreateDomain implements email.DomainManager.
func (p *Provider) CreateDomain(ctx context.Context, name string) (email.Domain, error) {
	resp, err := p.domainCall(ctx, http.MethodPost, "/domains", map[string]string{"name": name})
	if err != nil {
		return email.Domain{}, err
	}
	return p.domain(resp)
}

// VerifyDomain starts a check and reads the domain back. Resend checks
// asynchronously, so a fresh domain usually still reads as pending.
func (p *Provider) VerifyDomain(ctx context.Context, id string) (email.Domain, error) {
	path := "/domains/" + url.PathEscape(id)
	if resp, err := p.domainCall(ctx, http.MethodPost, path+"/verify", nil); err != nil {
		if resp.Status == http.StatusNotFound {
			return email.Domain{}, email.ErrDomainNotFound
		}
		return email.Domain{}, err
	}
	resp, err := p.domainCall(ctx, http.MethodGet, path, nil)
	if err != nil {
		if resp.Status == http.StatusNotFound {
			return email.Domain{}, email.ErrDomainNotFound
		}
		return email.Domain{}, err
	}
	return p.domain(resp)
}

// DeleteDomain implements email.DomainManager.
func (p *Provider) DeleteDomain(ctx context.Context, id string) error {
	resp, err := p.domainCall(ctx, http.MethodDelete, "/domains/"+url.PathEscape(id), nil)
	if resp.Status == http.StatusNotFound {
		return nil
	}
	return err
}
