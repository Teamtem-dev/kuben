package setup

// The text setup writes and reads back: the configuration file, the systemd
// units, and the answers it parses. Pure functions, tested without a
// machine.

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// dataHome is where a configuration keeps Kuben's data: this server's
// PostgreSQL (also when there is no configuration yet: setup writes one that
// uses it), the SQLite file of Kuben 1.x, or a PostgreSQL of the operator's
// own.
type dataHome int

const (
	dataLocal dataHome = iota
	dataSqlite
	dataExternal
)

func dataHomeOf(config opt.Val[string]) dataHome {
	text, ok := config.Get()
	if !ok {
		return dataLocal
	}
	url, found := "", false
	for _, line := range lines(text) {
		rest, ok := strings.CutPrefix(strings.TrimLeft(line, " \t"), "url")
		if !ok {
			continue
		}
		value, ok := strings.CutPrefix(strings.TrimLeft(rest, " \t"), "=")
		if !ok {
			continue
		}
		url, found = strings.Trim(strings.TrimSpace(value), `"`), true
		break
	}
	switch {
	case found && strings.HasPrefix(url, "sqlite:"):
		return dataSqlite
	case found && url != LocalDatabaseURL:
		return dataExternal
	default:
		return dataLocal
	}
}

// packages is how this distribution installs the PostgreSQL server, from
// /etc/os-release.
type packages int

const (
	packagesApt packages = iota
	packagesDnf
	packagesZypper
)

func packagesOf(osRelease string) (packages, bool) {
	ids := []string{}
	for _, line := range lines(osRelease) {
		value, ok := strings.CutPrefix(line, "ID=")
		if !ok {
			value, ok = strings.CutPrefix(line, "ID_LIKE=")
		}
		if ok {
			ids = append(ids, strings.Fields(strings.Trim(value, `"`))...)
		}
	}
	anyOf := func(names ...string) bool {
		for _, id := range ids {
			for _, n := range names {
				if id == n {
					return true
				}
			}
		}
		return false
	}
	switch {
	case anyOf("debian", "ubuntu"):
		return packagesApt, true
	case anyOf("fedora", "rhel", "centos"):
		return packagesDnf, true
	case anyOf("suse", "opensuse", "sles"):
		return packagesZypper, true
	default:
		return 0, false
	}
}

func (p packages) describe() string {
	switch p {
	case packagesApt:
		return "apt-get install postgresql"
	case packagesDnf:
		return "dnf install postgresql-server && postgresql-setup --initdb"
	case packagesZypper:
		return "zypper install postgresql-server"
	}
	return ""
}

// parsePort reads an answer to "Which port?": 1–65535.
func parsePort(answer string) (uint16, bool) {
	port, err := strconv.ParseUint(strings.TrimSpace(answer), 10, 16)
	if err != nil || port == 0 {
		return 0, false
	}
	return uint16(port), true
}

// configWants is what setup adds to the configuration beyond the basics.
type configWants struct {
	// hub is the address of this server the local agent dials.
	hub opt.Val[string]
	// console is the console's HTTPS host through Kuben's Gateway.
	console opt.Val[string]
	// insecureSetup is `--allow-http-setup`.
	insecureSetup bool
}

// securitySection is the `[security]` section: behind Kuben's Gateway, the
// forwarded headers of the pod network are believed (HTTPS, client
// address); with `--allow-http-setup` the first admin may be made over
// plain HTTP. Empty when neither applies.
func securitySection(wants configWants) string {
	ls := []string{}
	if wants.console.IsSome() {
		ls = append(ls,
			"# The console is reached through Kuben's Gateway; its proxies run in the pod network.",
			"trust_forwarded_for = true",
			fmt.Sprintf("trusted_proxies = [\"%s\"]", PodNetwork))
	}
	if wants.insecureSetup {
		ls = append(ls,
			"# kuben setup --allow-http-setup: the first admin may be made over plain HTTP.",
			"insecure_setup = true")
	}
	if len(ls) == 0 {
		return ""
	}
	return "\n[security]\n" + strings.Join(ls, "\n") + "\n"
}

// agentSection is the `[agent]` section of the local agent (M2.8): pods
// dial hub, an address of this server.
func agentSection(hub string) string {
	return fmt.Sprintf("\n[agent]\n"+
		"# The cluster agent in this server's k3s enrolls from what Kuben publishes.\n"+
		"bind = \"0.0.0.0:%[1]d\"\n"+
		"local = true\n"+
		"advertise = \"%[2]s:%[1]d\"\n"+
		"namespace = \"%[3]s\"\n", AgentPort, hub, GatewayNamespace)
}

func hasSection(text, name string) bool {
	for _, line := range lines(text) {
		if strings.TrimSpace(line) == "["+name+"]" {
			return true
		}
	}
	return false
}

// serverLines are the lines of the `[server]` section of a config file
// (and those before any section).
func serverLines(text string) []string {
	out := []string{}
	inServer := true
	for _, line := range lines(text) {
		key := strings.TrimSpace(line)
		if strings.HasPrefix(key, "[") {
			inServer = key == "[server]"
		}
		if inServer {
			out = append(out, line)
		}
	}
	return out
}

// configPort is the port of `bind` under `[server]` in a config file.
func configPort(text string) (uint16, bool) {
	for _, line := range serverLines(text) {
		rest, ok := strings.CutPrefix(strings.TrimLeft(line, " \t"), "bind")
		if !ok {
			continue
		}
		value, ok := strings.CutPrefix(strings.TrimLeft(rest, " \t"), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		port := value[strings.LastIndexByte(value, ':')+1:]
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return 0, false
		}
		return uint16(n), true
	}
	return 0, false
}

// withPort is text with the port of `bind` and `public_url` changed from
// old to new; every other line, comments included, stays as it is.
func withPort(text string, old, next uint16) string {
	from := fmt.Sprintf(":%d\"", old)
	to := fmt.Sprintf(":%d\"", next)
	inServer := true
	out := []string{}
	for _, line := range lines(text) {
		key := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(key, "[") {
			inServer = strings.TrimRight(key, " \t") == "[server]"
		}
		if inServer && (strings.HasPrefix(key, "bind") || strings.HasPrefix(key, "public_url")) {
			out = append(out, strings.ReplaceAll(line, from, to))
		} else {
			out = append(out, line)
		}
	}
	result := strings.Join(out, "\n")
	if strings.HasSuffix(text, "\n") {
		result += "\n"
	}
	return result
}

func configTemplate(bindHost string, port uint16, publicURL, kubeconfig string) string {
	return fmt.Sprintf("# Written by `kuben setup`. Every kuben command reads this file; restart the\n"+
		"# service after a change: systemctl restart kuben\n"+
		"\n"+
		"[server]\n"+
		"bind = \"%s:%d\"\n"+
		"metrics_bind = \"127.0.0.1:9090\"\n"+
		"# Set to the https:// address once Kuben sits behind TLS; the session cookie\n"+
		"# then becomes Secure on its own.\n"+
		"public_url = \"%s\"\n"+
		"# The setup token and a generated first admin password go here.\n"+
		"state_dir = \"%s\"\n"+
		"\n"+
		"[database]\n"+
		"# PostgreSQL on this server, over its Unix socket as the kuben user.\n"+
		"url = \"%s\"\n"+
		"\n"+
		"[kube]\n"+
		"kubeconfig = \"%s\"\n", bindHost, port, publicURL, StateDir, LocalDatabaseURL, kubeconfig)
}

// writtenBySetup reports whether setup wrote a unit file (also before it
// carried unitMarker).
func writtenBySetup(unit string) bool {
	return strings.HasPrefix(unit, unitMarker) || strings.Contains(unit, "ExecStart="+Bin+" serve")
}

func unitTemplate() string {
	return unitMarker + "; `kuben uninstall` removes it.\n" +
		"[Unit]\n" +
		"Description=Kuben\n" +
		"Documentation=" + Docs + "\n" +
		"After=network-online.target k3s.service postgresql.service\n" +
		"Wants=network-online.target postgresql.service\n" +
		"\n" +
		"[Service]\n" +
		"ExecStart=" + Bin + " serve\n" +
		"User=" + User + "\n" +
		"Group=" + User + "\n" +
		"StateDirectory=" + User + "\n" +
		"Environment=KUBEN_TELEMETRY__LOG_FORMAT=pretty\n" +
		"Restart=always\n" +
		"RestartSec=2\n" +
		"NoNewPrivileges=true\n" +
		"ProtectSystem=full\n" +
		"PrivateTmp=true\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
}

func backupServiceTemplate() string {
	return unitMarker + "; `kuben uninstall` removes it.\n" +
		"[Unit]\n" +
		"Description=Kuben database backup\n" +
		"Documentation=" + Docs + "\n" +
		"After=network-online.target postgresql.service\n" +
		"\n" +
		"[Service]\n" +
		"Type=oneshot\n" +
		"ExecStart=" + Bin + " backup --scheduled\n" +
		"User=" + User + "\n" +
		"Group=" + User + "\n" +
		"StateDirectory=" + User + "\n" +
		"Nice=10\n" +
		"IOSchedulingClass=idle\n" +
		"NoNewPrivileges=true\n" +
		"ProtectSystem=full\n" +
		"PrivateTmp=true\n"
}

func backupTimerTemplate() string {
	return unitMarker + "; `kuben uninstall` removes it.\n" +
		"[Unit]\n" +
		"Description=Daily Kuben database backup\n" +
		"\n" +
		"[Timer]\n" +
		"OnCalendar=*-*-* 03:17:00\n" +
		"RandomizedDelaySec=15min\n" +
		"Persistent=true\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=timers.target\n"
}

// firewall is the host firewall that is active.
type firewall int

const (
	firewallUfw firewall = iota
	firewallFirewalld
)

// opening is a TCP port setup opens, from anywhere or from one network
// only.
type opening struct {
	port   uint16
	source opt.Val[string]
}

func (f firewall) tool() string {
	switch f {
	case firewallUfw:
		return "ufw"
	case firewallFirewalld:
		return "firewalld"
	}
	return ""
}

// rule is the journal's name of the rule, e.g. `ufw:3000/tcp` or
// `ufw:10.42.0.0/16:7443/tcp`.
func (f firewall) rule(o opening) string {
	if source, ok := o.source.Get(); ok {
		return fmt.Sprintf("%s:%s:%d/tcp", f.tool(), source, o.port)
	}
	return fmt.Sprintf("%s:%d/tcp", f.tool(), o.port)
}

// parseRule is the firewall and opening a journal rule names.
func parseRule(rule string) (firewall, opening, bool) {
	parts := strings.Split(rule, ":")
	var f firewall
	switch parts[0] {
	case "ufw":
		f = firewallUfw
	case "firewalld":
		f = firewallFirewalld
	default:
		return 0, opening{}, false
	}
	var source opt.Val[string]
	var port string
	switch rest := parts[1:]; len(rest) {
	case 1:
		port = rest[0]
	case 2:
		source, port = opt.Some(rest[0]), rest[1]
	default:
		return 0, opening{}, false
	}
	digits, ok := strings.CutSuffix(port, "/tcp")
	if !ok {
		return 0, opening{}, false
	}
	n, err := strconv.ParseUint(digits, 10, 16)
	if err != nil {
		return 0, opening{}, false
	}
	return f, opening{port: uint16(n), source: source}, true
}

func richRule(o opening) string {
	return fmt.Sprintf("rule family=ipv4 source address=%s port port=%d protocol=tcp accept", o.source.Or("0.0.0.0/0"), o.port)
}

// withExtension is Rust's Path::with_extension: the extension of the last
// component replaced by ext.
func withExtension(path, ext string) string {
	dir, base := filepath.Split(path)
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return dir + base + "." + ext
}

// lines splits text as Rust's str::lines: on `\n`, a trailing `\r`
// dropped, no empty last line for a trailing newline.
func lines(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}
