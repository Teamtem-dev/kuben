package setup

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
)

// sslip.io gives a server without a domain of its own one: every name under
// <a-b-c-d>.sslip.io resolves to a.b.c.d, so Let's Encrypt can issue the
// console's and the apps' certificates (HTTP-01 through Kuben's Gateway)
// with no DNS record to make.
const sslipSuffix = ".sslip.io"

// sslipDomain is the sslip.io base domain of ip, when ip is a public IPv4
// address: Let's Encrypt must reach it.
func sslipDomain(ip string) opt.Val[string] {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(addr) { // carrier-grade NAT
		return opt.None[string]()
	}
	return opt.Some(strings.ReplaceAll(addr.String(), ".", "-") + sslipSuffix)
}

// configuredSslip is the sslip.io base domain an earlier run chose, read
// from the console's public_url (https://kuben.<domain>).
func configuredSslip(configured opt.Val[string]) opt.Val[string] {
	text, ok := configured.Get()
	if !ok {
		return opt.None[string]()
	}
	for _, line := range lines(text) {
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != "public_url" {
			continue
		}
		url := strings.TrimRight(strings.Trim(strings.TrimSpace(value), `"`), "/")
		if domain, cut := strings.CutPrefix(url, "https://kuben."); cut && strings.HasSuffix(domain, sslipSuffix) {
			return opt.Some(domain)
		}
	}
	return opt.None[string]()
}

// certificateWait is how long setup waits for the console's certificate:
// Let's Encrypt usually takes a minute.
const certificateWait = 3 * time.Minute

// waitForCertificate holds the link back until the console's certificate
// is issued, so that it opens the moment it is printed. DNS is there from
// the start under sslip.io; on a domain of one's own it may take hours,
// and setup does not wait for it.
func (m *machine) waitForCertificate(ctx context.Context, c cluster, host string) {
	st := m.ui.Step("The HTTPS certificate")
	defer st.Close()
	issued := m.eventually(ctx, certificateWait, func() bool {
		cert, found, err := c.get(ctx, "cert-manager.io/v1", "Certificate", GatewayNamespace, render.HostSecretName(host))
		return err == nil && found && cert != nil && conditionTrue(cert.Object, "Ready", "status", "conditions")
	})
	if issued {
		st.Done("issued by Let's Encrypt")
		return
	}
	st.Warn("not issued yet: Let's Encrypt must reach ports 80 and 443 of this server; cert-manager keeps trying")
}

// isSslip reports whether domain is an sslip.io domain.
func isSslip(domain string) bool { return strings.HasSuffix(domain, sslipSuffix) }

// withSslip is opts with an sslip.io base domain and HTTPS, for a server
// without a domain: the one an earlier run chose, with --sslip, or on the
// operator's answer (Enter says yes; with --yes or without a terminal, no).
// The Let's Encrypt account then needs no email unless --acme-email gives
// one.
func (m *machine) withSslip(opts Opts, hub, configured opt.Val[string]) Opts {
	if opts.Domain.IsSome() || opts.BindLocal {
		return opts
	}
	domain, ok := configuredSslip(configured).Get()
	if !ok {
		d, public := sslipDomain(hub.Or("")).Get()
		switch {
		case !public && opts.Sslip:
			m.ui.Warn("sslip.io", "this server has no public IPv4 address Let's Encrypt can reach; "+
				"use --domain with a domain of your own")
			return opts
		case !public:
			return opts
		case !opts.Sslip && opts.Yes:
			return opts
		case !opts.Sslip:
			yes, asked := m.ui.ConfirmDefaultYes(fmt.Sprintf(
				"Serve the console over HTTPS at https://kuben.%s (a free sslip.io name, Let's Encrypt)?", d))
			if !asked || !yes {
				return opts
			}
		}
		domain = d
	}
	opts.Domain = opt.Some(domain)
	if opts.AcmeEmail.IsNone() {
		opts.AcmeEmail = opt.Some("") // an account without an email
	}
	return opts
}
