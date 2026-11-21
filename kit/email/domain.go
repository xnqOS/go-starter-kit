package email

import (
	"context"
	"errors"
	"strings"
)

// ErrDomainNotFound means the provider has no domain with that ID (it was
// deleted there). VerifyDomain returns it only for a definite 404, never for
// transient failures.
var ErrDomainNotFound = errors.New("domain not found at the provider")

// Domain statuses, as far as we care: the provider either has verified the
// DNS records or not yet.
const (
	DomainPending  = "pending"
	DomainVerified = "verified"
	DomainFailed   = "failed"
)

// DNSRecord is one record the domain owner must add.
type DNSRecord struct {
	// Purpose is a short label such as "dkim", "spf" or "return_path".
	Purpose  string `json:"purpose,omitempty"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
	// Verified is the provider's view of this record, when it reports one.
	Verified bool `json:"verified"`
}

// Domain is a sending domain at one provider.
type Domain struct {
	ID      string
	Status  string
	Records []DNSRecord
}

// DomainManager is implemented by adapters that can register sending domains.
// Each method talks to one provider and only accepts that provider's IDs.
type DomainManager interface {
	// CreateDomain registers name and returns the DNS records to publish.
	CreateDomain(ctx context.Context, name string) (Domain, error)
	// VerifyDomain asks the provider to check DNS now and returns the result,
	// or ErrDomainNotFound when the provider no longer has the domain.
	VerifyDomain(ctx context.Context, id string) (Domain, error)
	// DeleteDomain removes the domain; an unknown ID is not an error.
	DeleteDomain(ctx context.Context, id string) error
}

// Domains returns p's DomainManager, if it has one.
func Domains(p Provider) (DomainManager, bool) {
	m, ok := p.(DomainManager)
	return m, ok
}

// Log mode pretends every domain verifies at once, so a domain setup flow can be
// tried locally without DNS.
func (Log) CreateDomain(_ context.Context, name string) (Domain, error) {
	return Domain{ID: "log-" + name, Status: DomainPending, Records: []DNSRecord{
		{Purpose: "dkim", Type: "TXT", Name: "log._domainkey." + name, Value: "p=development-only"},
		{Purpose: "spf", Type: "TXT", Name: name, Value: "v=spf1 -all"},
	}}, nil
}

func (l Log) VerifyDomain(ctx context.Context, id string) (Domain, error) {
	d, err := l.CreateDomain(ctx, strings.TrimPrefix(id, "log-"))
	d.Status = DomainVerified
	for i := range d.Records {
		d.Records[i].Verified = true
	}
	return d, err
}

func (Log) DeleteDomain(context.Context, string) error { return nil }
