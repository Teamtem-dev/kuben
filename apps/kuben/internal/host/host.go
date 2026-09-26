// Package host is the machine Kuben runs on (crates/kuben-api/src/host.rs):
// the address others reach it at, and files only its own user may read.
package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// AdvertiseIP is the address other machines reach this one at: the source
// address of a route towards a public IP. Nothing is sent (a UDP "connect"
// only picks the route). None without a route, e.g. offline.
func AdvertiseIP(ctx context.Context) opt.Val[string] {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp4", "1.1.1.1:53")
	if err != nil {
		return opt.None[string]()
	}
	defer conn.Close() //nolint:errcheck // nothing was sent
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP.IsLoopback() || addr.IP.IsUnspecified() {
		return opt.None[string]()
	}
	return opt.Some(addr.IP.String())
}

// ConsoleURL is the console's address for links: `server.public_url`,
// else this machine's address and the bound port.
func ConsoleURL(ctx context.Context, cfg config.Config) string {
	return cfg.ConsoleURLWithHost(AdvertiseIP(ctx).Or("localhost"))
}

// WriteOwnerOnly writes content to path as a new file readable by its owner
// only. An existing file is replaced, so a leftover never keeps a wider
// mode.
func WriteOwnerOnly(path, content string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the caller's own file
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close() //nolint:errcheck // the write error is the one to report
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
