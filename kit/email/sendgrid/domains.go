package sendgrid

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

type dnsEntry struct {
	Valid bool   `json:"valid"`
	Type  string `json:"type"`
	Host  string `json:"host"`
	Data  string `json:"data"`
}

type domainReply struct {
	ID    int64               `json:"id"`
	Valid bool                `json:"valid"`
	DNS   map[string]dnsEntry `json:"dns"`
}

func (p *Provider) domainCall(ctx context.Context, method, path string, body any) (email.HTTPResponse, error) {
	header := http.Header{"Authorization": {"Bearer " + p.config.APIKey}}
	resp, err := email.DoJSON(ctx, p.client, method, p.baseURL+path, header, body)
	if err != nil {
		return resp, err
	}
	if resp.Status/100 != 2 {
		return resp, email.StatusError(email.ClassifyStatus(resp.Status), resp.Status, "")
	}
	return resp, nil
}

func decodeDomain(resp email.HTTPResponse) (email.Domain, error) {
	var reply domainReply
	if err := json.Unmarshal(resp.Body, &reply); err != nil || reply.ID == 0 {
		return email.Domain{}, email.Failed(email.Permanent, "decode", err)
	}
	out := email.Domain{ID: strconv.FormatInt(reply.ID, 10), Status: email.DomainPending}
	if reply.Valid {
		out.Status = email.DomainVerified
	}
	// Fixed order, so the page shows the records the same way every time.
	for _, key := range []string{"mail_cname", "dkim1", "dkim2", "mail_server", "subdomain_spf", "dkim"} {
		entry, ok := reply.DNS[key]
		if !ok {
			continue
		}
		purpose := "return_path"
		if strings.HasPrefix(key, "dkim") {
			purpose = "dkim"
		} else if strings.Contains(key, "spf") {
			purpose = "spf"
		}
		out.Records = append(out.Records, email.DNSRecord{Purpose: purpose, Type: strings.ToUpper(entry.Type), Name: entry.Host, Value: entry.Data, Verified: entry.Valid})
	}
	return out, nil
}

// CreateDomain implements email.DomainManager (Domain Authentication).
func (p *Provider) CreateDomain(ctx context.Context, name string) (email.Domain, error) {
	resp, err := p.domainCall(ctx, http.MethodPost, "/v3/whitelabel/domains", map[string]any{"domain": name, "automatic_security": true})
	if err != nil {
		return email.Domain{}, err
	}
	return decodeDomain(resp)
}

// VerifyDomain asks SendGrid to validate the records, then reads them back.
func (p *Provider) VerifyDomain(ctx context.Context, id string) (email.Domain, error) {
	path := "/v3/whitelabel/domains/" + url.PathEscape(id)
	if resp, err := p.domainCall(ctx, http.MethodPost, path+"/validate", nil); err != nil {
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
	return decodeDomain(resp)
}

// DeleteDomain implements email.DomainManager.
func (p *Provider) DeleteDomain(ctx context.Context, id string) error {
	resp, err := p.domainCall(ctx, http.MethodDelete, "/v3/whitelabel/domains/"+url.PathEscape(id), nil)
	if resp.Status == http.StatusNotFound {
		return nil
	}
	return err
}
