package api

// An app's DNS records, written through a DNS provider account (M5.2,
// routes/domains.rs sync_app).

import (
	"context"
	"net/netip"

	domain "github.com/Teamtem-dev/kuben/internal/core/dnsname"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/doctor"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/integrations/dns"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// gatewayAddresses are the addresses records point at: the Gateway's
// public ones.
func (s *Server) gatewayAddresses(ctx context.Context) ([]netip.Addr, error) {
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	platform := doctor.ReadPlatform(ctx, cluster.Dynamic)
	return doctor.ReadGateway(ctx, cluster.Dynamic, platform, s.deps.Resolver).Addresses(), nil
}

// changeDto is one change of a pass; the record type and content are
// those of spec, when there is one.
func changeDto(host, action string, spec opt.Val[dns.RecordSpec], detail opt.Val[string]) gen.DnsChangeDto {
	recordType, content := opt.None[string](), opt.None[string]()
	if s, ok := spec.Get(); ok {
		recordType, content = opt.Some(s.RecordType), opt.Some(s.Content)
	}
	return gen.DnsChangeDto{
		Host: host, RecordType: optNilString(recordType), Content: optNilString(content),
		Action: action, Detail: optNilString(detail),
	}
}

// hostSkipped is a host the pass left alone, and why.
func hostSkipped(host, why string) gen.DnsChangeDto {
	return changeDto(host, "skipped", opt.None[dns.RecordSpec](), opt.Some(why))
}

// hostFailed is a host whose provider failed.
func hostFailed(host string, err error) gen.DnsChangeDto {
	return changeDto(host, "failed", opt.None[dns.RecordSpec](), opt.Some(err.Error()))
}

// dnsPass is one pass over an app's hosts through one provider account.
type dnsPass struct {
	t        *store.Tenant
	provider openedProvider
	target   ids.TargetID
	tag      string
}

// written is what one change did at the provider: the action, the spec
// and the record to remember (none after a delete).
type written struct {
	action string
	spec   opt.Val[dns.RecordSpec]
	record opt.Val[dns.ProviderRecord]
	err    error
}

// carryOut sends change to the provider; a deleted record is forgotten
// when known recorded it. False for a conflict, which sends nothing.
func (p dnsPass) carryOut(ctx context.Context, zone dns.Zone, change dns.Change, known []store.DNSRecord) (written, bool, error) {
	api := p.provider.api
	switch c := change.(type) {
	case dns.Create:
		record, err := api.Create(ctx, zone, c.Spec, p.tag)
		return written{"created", opt.Some(c.Spec), opt.Some(record), err}, true, nil
	case dns.Update:
		record, err := api.Update(ctx, zone, c.ID, c.Spec, p.tag)
		return written{"updated", opt.Some(c.Spec), opt.Some(record), err}, true, nil
	case dns.Unchanged:
		record := dns.ProviderRecord{
			ID: c.ID, Name: c.Spec.Name, RecordType: c.Spec.RecordType, Content: c.Spec.Content,
			Comment: opt.Some(p.tag),
		}
		return written{"unchanged", opt.Some(c.Spec), opt.Some(record), nil}, true, nil
	case dns.Delete:
		err := api.Delete(ctx, zone, c.ID)
		if err == nil {
			for _, r := range known {
				if ref, ok := r.ProviderRef.Get(); ok && ref == c.ID {
					if err := p.t.ForgetDNSRecord(ctx, r.ID); err != nil {
						return written{}, false, err //nolint:wrapcheck // a store error, answered as internal
					}
				}
			}
		}
		return written{"deleted", opt.None[dns.RecordSpec](), opt.None[dns.ProviderRecord](), err}, true, nil
	case dns.ConflictWith, nil:
	}
	return written{}, false, nil
}

// apply carries out changes for host in zone, recording what was written.
func (p dnsPass) apply(ctx context.Context, zone dns.Zone, host string, changes []dns.Change) ([]gen.DnsChangeDto, error) {
	known, err := p.t.DNSRecords(ctx, p.target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make([]gen.DnsChangeDto, 0, len(changes))
	for _, change := range changes {
		if c, isConflict := change.(dns.ConflictWith); isConflict {
			conflict := dns.Error{Kind: dns.Conflict, Detail: c.What}
			out = append(out, changeDto(host, "conflict", opt.None[dns.RecordSpec](), opt.Some(conflict.Error())))
			continue
		}
		w, sent, err := p.carryOut(ctx, zone, change, known)
		if err != nil {
			return nil, err
		}
		if !sent {
			continue
		}
		if w.err != nil {
			out = append(out, changeDto(host, "failed", w.spec, opt.Some(w.err.Error())))
			continue
		}
		if record, ok := w.record.Get(); ok {
			if _, err := p.t.RecordDNS(ctx, store.NewDNSRecord{
				Provider: p.provider.id, Target: opt.Some(p.target), Name: record.Name,
				RecordType: record.RecordType, Content: record.Content, ZoneID: zone.ID, ProviderRef: record.ID,
			}); err != nil {
				return nil, err //nolint:wrapcheck // a store error, answered as internal
			}
		}
		out = append(out, changeDto(host, w.action, w.spec, opt.None[string]()))
	}
	return out, nil
}

// appDomainHosts are the custom domains of app: its desired spec's, else
// the `domains[].host` of its configuration.
func appDomainHosts(app store.AppRecord) []string {
	var hosts []string
	if spec, ok := desiredSpec(app); ok {
		for _, d := range spec.Domains {
			hosts = append(hosts, d.Host)
		}
		return hosts
	}
	config, isObject := app.Config.Or(nil).(map[string]any)
	if !isObject {
		return hosts
	}
	list, isList := config["domains"].([]any)
	if !isList {
		return hosts
	}
	for _, d := range list {
		entry, isEntry := d.(map[string]any)
		if !isEntry {
			continue
		}
		if host, isText := entry["host"].(string); isText {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// syncHost writes the records of one host; the changes it made or why not.
func (s *Server) syncHost(ctx context.Context, p dnsPass, org ids.OrgID, host string, addresses []netip.Addr) ([]gen.DnsChangeDto, error) {
	_, owner, found, err := s.deps.Store.DomainOwner(ctx, host)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found || owner != org {
		return []gen.DnsChangeDto{hostSkipped(host, "the domain is not verified for this organization")}, nil
	}
	wanted := dns.GatewayRecords(host, addresses, s.deps.Config.Domains.CnameTarget)
	if len(wanted) == 0 {
		return []gen.DnsChangeDto{hostSkipped(host, "the Gateway has no public address yet")}, nil
	}
	api := p.provider.api
	zone, hasZone, err := api.ZoneFor(ctx, host)
	switch {
	case err != nil:
		return []gen.DnsChangeDto{hostFailed(host, err)}, nil
	case !hasZone:
		return []gen.DnsChangeDto{hostSkipped(host, "the provider holds no zone for it")}, nil
	}
	existing, err := api.Records(ctx, zone, host)
	if err != nil {
		return []gen.DnsChangeDto{hostFailed(host, err)}, nil
	}
	recorded, err := p.t.DNSRecords(ctx, p.target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	var known []string
	for _, r := range recorded {
		if ref, ok := r.ProviderRef.Get(); ok && r.Name == host {
			known = append(known, ref)
		}
	}
	return p.apply(ctx, zone, host, dns.Plan(wanted, existing, known, p.tag))
}

// SyncAppDns writes an app's DNS records: every custom domain the
// organization verified points at the Gateway (or domains.cname_target).
func (s *Server) SyncAppDns(ctx context.Context, req *gen.SyncDns, params gen.SyncAppDnsParams) (gen.SyncAppDnsRes, error) { //nolint:revive // the generated interface's name
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppWrite, a.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	org := a.env.project.org
	hosts := appDomainHosts(a.app)
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	provider, err := s.provider(ctx, t, org, req.Provider)
	if err != nil {
		return nil, err
	}
	var addresses []netip.Addr
	if s.deps.Config.Domains.CnameTarget.IsNone() {
		if addresses, err = s.gatewayAddresses(ctx); err != nil {
			return nil, err
		}
	}
	pass := dnsPass{t: t, provider: provider, target: a.app.Target, tag: "kuben:" + org.String()}
	out := []gen.DnsChangeDto{}
	for _, name := range hosts {
		host, err := domain.Canonical(name)
		if err != nil {
			continue
		}
		changes, err := s.syncHost(ctx, pass, org, host, addresses)
		if err != nil {
			return nil, err
		}
		out = append(out, changes...)
	}
	reference := params.Project + "/" + params.Environment + "/" + params.App
	if err := t.AppendAudit(ctx, requestAudit(acc, "app.dns.synced", "app", reference)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	res := gen.SyncAppDnsOKApplicationJSON(out)
	return &res, nil
}
