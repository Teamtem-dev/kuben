package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/dnsname"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/outbound"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/version"
)

// Resolver is a DNS-over-HTTPS resolver (the JSON API of Cloudflare and
// Google).
type Resolver struct {
	client *outbound.Client
	url    string
}

// NewResolver asks the resolver at url, over https only.
func NewResolver(url string) Resolver {
	return Resolver{client: outbound.New(false, timeout), url: strings.TrimRight(url, "/")}
}

func (r Resolver) query(ctx context.Context, name string, kind uint64) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?name=%s&type=%d", r.url, name, kind), nil)
	if err != nil {
		return nil, unavailable("%v", err)
	}
	req.Header.Set("Accept", "application/dns-json")
	status, body, err := r.client.Fetch(ctx, req, maxBody)
	if err != nil {
		return nil, unavailable("%v", err)
	}
	if status < 200 || status > 299 {
		return nil, unavailable("the resolver answered HTTP %s", statusText(status))
	}
	return DoHAnswers(body, kind)
}

// TXT is the TXT values of name.
func (r Resolver) TXT(ctx context.Context, name string) ([]string, error) {
	return r.query(ctx, name, TypeTXT)
}

// NS is the name servers of name.
func (r Resolver) NS(ctx context.Context, name string) ([]string, error) {
	return r.query(ctx, name, TypeNS)
}

// statusText is a status as the http crate's StatusCode displays it
// (`404 Not Found`).
func statusText(status int) string {
	return strings.TrimSpace(fmt.Sprintf("%d %s", status, http.StatusText(status)))
}

// Provider is a DNS provider account, as far as Kuben needs it.
type Provider interface {
	// Verify checks the credentials.
	Verify(ctx context.Context) error
	// ZoneFor is the zone holding name, if the account has one.
	ZoneFor(ctx context.Context, name string) (Zone, bool, error)
	// Records are the records named name in zone.
	Records(ctx context.Context, zone Zone, name string) ([]ProviderRecord, error)
	// Create writes spec, tagged tag.
	Create(ctx context.Context, zone Zone, spec RecordSpec, tag string) (ProviderRecord, error)
	// Update rewrites record id as spec, tagged tag.
	Update(ctx context.Context, zone Zone, id string, spec RecordSpec, tag string) (ProviderRecord, error)
	// Delete removes record id.
	Delete(ctx context.Context, zone Zone, id string) error
}

// Cloudflare is the Cloudflare API with an API token.
type Cloudflare struct {
	client *outbound.Client
	api    string
	token  string
}

// NewCloudflare is the API at api (plain http allowed only for an http://
// URL, as tests use) with token.
func NewCloudflare(api, token string) *Cloudflare {
	return &Cloudflare{
		client: outbound.New(strings.HasPrefix(api, "http://"), timeout),
		api:    strings.TrimRight(api, "/"),
		token:  token,
	}
}

// String never shows the token.
func (c *Cloudflare) String() string { return fmt.Sprintf("Cloudflare { api: %q, .. }", c.api) }

// call sends one request and reads the result of Cloudflare's envelope.
func (c *Cloudflare) call(ctx context.Context, method, path string, body any, result any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return unavailable("%v", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, bytes.NewReader(payload))
	if err != nil {
		return unavailable("%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kuben/"+version.Version)
	status, answer, err := c.client.Fetch(ctx, req, maxBody)
	if err != nil {
		return unavailable("%v", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return Error{Kind: Refused, Detail: "HTTP " + statusText(status)}
	}
	var envelope struct {
		Success bool              `json:"success"`
		Errors  []json.RawMessage `json:"errors"`
		Result  json.RawMessage   `json:"result"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		return unavailable("HTTP %s: unexpected answer: %v", statusText(status), err)
	}
	if !envelope.Success || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return unavailable("HTTP %s: %s", statusText(status), errorsText(envelope.Errors))
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return unavailable("HTTP %s: unexpected answer: %v", statusText(status), err)
	}
	return nil
}

// errorsText is Cloudflare's errors as serde_json printed the array
// (compact, keys sorted).
func errorsText(errs []json.RawMessage) string {
	if errs == nil {
		return "[]"
	}
	raw, err := json.Marshal(errs)
	if err != nil {
		return "[]"
	}
	text, err := jsonx.Canonical(raw)
	if err != nil {
		return string(raw)
	}
	return text
}

// QueryValue percent-encodes everything but letters, digits, `-`, `.`
// and `_`, as dns.rs query_value did.
func QueryValue(value string) string {
	var b strings.Builder
	for _, c := range []byte(value) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Verify checks that the token is active.
func (c *Cloudflare) Verify(ctx context.Context) error {
	var token struct {
		Status string `json:"status"`
	}
	if err := c.call(ctx, http.MethodGet, "/user/tokens/verify", nil, &token); err != nil {
		return err
	}
	if token.Status != "active" {
		return Error{Kind: Refused, Detail: "the token is " + token.Status}
	}
	return nil
}

// ZoneFor is the account's zone holding name, looked for from the name
// itself up to its registrable parent.
func (c *Cloudflare) ZoneFor(ctx context.Context, name string) (Zone, bool, error) {
	for _, candidate := range dnsname.Ancestors(name) {
		var zones []Zone
		if err := c.call(ctx, http.MethodGet, "/zones?name="+QueryValue(candidate), nil, &zones); err != nil {
			return Zone{}, false, err
		}
		if len(zones) > 0 {
			return zones[0], true, nil
		}
	}
	return Zone{}, false, nil
}

// Records are the zone's records named name (the first 100).
func (c *Cloudflare) Records(ctx context.Context, zone Zone, name string) ([]ProviderRecord, error) {
	var records []ProviderRecord
	path := fmt.Sprintf("/zones/%s/dns_records?name=%s&per_page=100", QueryValue(zone.ID), QueryValue(name))
	return records, c.call(ctx, http.MethodGet, path, nil, &records)
}

func recordBody(spec RecordSpec, tag string) map[string]any {
	return map[string]any{
		"type": spec.RecordType, "name": spec.Name, "content": spec.Content, "ttl": 1, "proxied": false, "comment": tag,
	}
}

// Create writes spec, not proxied, with automatic TTL, tagged tag.
func (c *Cloudflare) Create(ctx context.Context, zone Zone, spec RecordSpec, tag string) (ProviderRecord, error) {
	var record ProviderRecord
	path := fmt.Sprintf("/zones/%s/dns_records", QueryValue(zone.ID))
	return record, c.call(ctx, http.MethodPost, path, recordBody(spec, tag), &record)
}

// Update rewrites record id as spec, tagged tag.
func (c *Cloudflare) Update(ctx context.Context, zone Zone, id string, spec RecordSpec, tag string) (ProviderRecord, error) {
	var record ProviderRecord
	path := fmt.Sprintf("/zones/%s/dns_records/%s", QueryValue(zone.ID), QueryValue(id))
	return record, c.call(ctx, http.MethodPut, path, recordBody(spec, tag), &record)
}

// Delete removes record id.
func (c *Cloudflare) Delete(ctx context.Context, zone Zone, id string) error {
	var ignored json.RawMessage
	path := fmt.Sprintf("/zones/%s/dns_records/%s", QueryValue(zone.ID), QueryValue(id))
	return c.call(ctx, http.MethodDelete, path, nil, &ignored)
}

// Backend is where lookups and provider accounts come from; tests replace
// it.
type Backend interface {
	// TXT is the TXT values of name.
	TXT(ctx context.Context, name string) ([]string, error)
	// NS is the name servers of name.
	NS(ctx context.Context, name string) ([]string, error)
	// Provider is the provider of kind with token; false for an unknown
	// kind.
	Provider(kind, token string) (Provider, bool)
}

// Public is the public DNS, through DNS-over-HTTPS, and the real providers.
type Public struct {
	resolver      Resolver
	cloudflareAPI string
}

// NewPublic is the public DNS as the configuration names it.
func NewPublic(cfg config.DomainsCfg) Public {
	return Public{resolver: NewResolver(cfg.DohURL), cloudflareAPI: cfg.CloudflareAPIURL}
}

// TXT asks the resolver.
func (p Public) TXT(ctx context.Context, name string) ([]string, error) {
	return p.resolver.TXT(ctx, name)
}

// NS asks the resolver.
func (p Public) NS(ctx context.Context, name string) ([]string, error) {
	return p.resolver.NS(ctx, name)
}

// Provider is the account of kind: only "cloudflare" so far.
func (p Public) Provider(kind, token string) (Provider, bool) {
	if kind == "cloudflare" {
		return NewCloudflare(p.cloudflareAPI, token), true
	}
	return nil, false
}
