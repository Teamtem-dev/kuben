package outbound

// Connections to services an organization chose (2.1): a Git connection's
// provider, an organization registry. Their addresses come from an
// organization admin, not the operator, so unless the operator allows it
// the server does not connect to one inside its own network.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Resolver looks up the addresses of a host name; *net.Resolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ContextDialer opens connections; *net.Dialer is one.
type ContextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// PrivateAddress is why a connection was refused: its host is, or resolves
// to, an address outside the public internet ([IsPrivate]).
type PrivateAddress struct {
	Host string
	Addr netip.Addr
}

func (e PrivateAddress) Error() string {
	if e.Host == e.Addr.String() {
		return fmt.Sprintf("%s is a private address; the server connects to public addresses only "+
			"unless integrations.allow_private_hosts is set", e.Host)
	}
	return fmt.Sprintf("%s resolves to a private address (%s); the server connects to public addresses only "+
		"unless integrations.allow_private_hosts is set", e.Host, e.Addr)
}

// Guard dials public addresses only. It resolves the host itself, refuses
// it when any of its addresses is private, and connects to the addresses
// it checked: a name that resolves elsewhere between the check and the
// connection (DNS rebinding) changes nothing. The zero Guard uses the
// system's resolver and a dialer like http.DefaultTransport's.
type Guard struct {
	Resolver Resolver
	Dialer   ContextDialer
}

func (g Guard) resolver() Resolver {
	if g.Resolver == nil {
		return net.DefaultResolver
	}
	return g.Resolver
}

func (g Guard) dialer() ContextDialer {
	if g.Dialer == nil {
		return &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	}
	return g.Dialer
}

// Check resolves host and returns its addresses, or a [PrivateAddress]
// when any of them is private.
func (g Guard) Check(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if IsPrivate(ip) {
			return nil, PrivateAddress{Host: host, Addr: ip}
		}
		return []netip.Addr{ip}, nil
	}
	addrs, err := g.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("cannot resolve %s: no addresses", host)
	}
	for _, addr := range addrs {
		if IsPrivate(addr) {
			return nil, PrivateAddress{Host: host, Addr: addr}
		}
	}
	return addrs, nil
}

// DialContext connects to address (`host:port`) through one of the public
// addresses its host resolves to, in the resolver's order.
func (g Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	addrs, err := g.Check(ctx, host)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, addr := range addrs {
		conn, err := g.dialer().DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%w", errors.Join(errs...))
}

// NewTransport is a clone of http.DefaultTransport that takes the proxy
// from the environment and, unless allowPrivate, connects through g only.
// A proxy the environment names is the operator's and is dialed as it is;
// since it resolves the target itself, the target is checked with g before
// the request goes to the proxy (the proxy's own rules are what stands
// between the check and its connection).
func NewTransport(allowPrivate bool, g Guard) *http.Transport {
	return newTransport(allowPrivate, g, http.ProxyFromEnvironment)
}

// newTransport is NewTransport with the proxies of proxyFor.
func newTransport(allowPrivate bool, g Guard, proxyFor func(*http.Request) (*url.URL, error)) *http.Transport {
	t := &http.Transport{}
	if d, ok := http.DefaultTransport.(*http.Transport); ok {
		t = d.Clone()
	}
	t.Proxy = proxyFor
	if allowPrivate {
		return t
	}
	var proxies sync.Map
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		proxy, err := proxyFor(req)
		if err != nil || proxy == nil {
			return proxy, err //nolint:wrapcheck // net/http's own
		}
		if _, err := g.Check(req.Context(), req.URL.Hostname()); err != nil {
			return nil, err
		}
		proxies.Store(proxyAddr(proxy), struct{}{})
		return proxy, nil
	}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if _, isProxy := proxies.Load(address); isProxy {
			return g.dialer().DialContext(ctx, network, address) //nolint:wrapcheck // net's own
		}
		return g.DialContext(ctx, network, address)
	}
	return t
}

// proxyAddr is the `host:port` net/http dials for proxy.
func proxyAddr(proxy *url.URL) string {
	if port := proxy.Port(); port != "" {
		return net.JoinHostPort(proxy.Hostname(), port)
	}
	port := "80"
	switch proxy.Scheme {
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	}
	return net.JoinHostPort(proxy.Hostname(), port)
}
