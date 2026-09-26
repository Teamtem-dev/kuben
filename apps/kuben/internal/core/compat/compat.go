// Package compat has the support envelope (M4.12; plan §18.5): what a
// Supported MVP installation runs on, and what is claimed about it. It
// replaces the Rust module kuben-core/src/support.rs.
//
// `doctor` checks an installation against it and a support bundle carries
// it. A test keeps the bundle lock inside it. A version outside
// [VersionRange.Tested] can still work ("untested"). A version below
// [VersionRange.WorksFrom] is unsupported.
package compat

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
)

// Minor is a `major.minor` version.
type Minor struct {
	Major uint32
	Minor uint32
}

// String is `major.minor`.
func (m Minor) String() string { return fmt.Sprintf("%d.%d", m.Major, m.Minor) }

// Compare orders versions by major, then minor.
func (m Minor) Compare(other Minor) int {
	if m.Major != other.Major {
		return compareUint32(m.Major, other.Major)
	}
	return compareUint32(m.Minor, other.Minor)
}

func compareUint32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// ParseMinor is the `major.minor` of a version string such as
// `v1.36.4+k3s1`, `1.36`, `17.4` or `17`. A missing or unreadable minor is 0;
// without a major there is no version.
func ParseMinor(version string) (Minor, bool) {
	v := strings.TrimLeft(strings.TrimSpace(version), "v")
	majorText, rest := cutSeparator(v)
	major, ok := parseUint32(majorText)
	if !ok {
		return Minor{}, false
	}
	minorText, _ := cutSeparator(rest)
	minor, _ := parseUint32(leadingDigits(minorText))
	return Minor{Major: major, Minor: minor}, true
}

// parseUint32 reads decimal digits that fit 32 bits.
func parseUint32(s string) (uint32, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n > math.MaxUint32 {
		return 0, false
	}
	return uint32(n), true
}

// cutSeparator cuts s at its first `.`, `-` or `+`.
func cutSeparator(s string) (before, after string) {
	at := strings.IndexAny(s, ".-+")
	if at < 0 {
		return s, ""
	}
	return s[:at], s[at+1:]
}

// leadingDigits is the run of digits s starts with.
func leadingDigits(s string) string {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return s[:end]
}

// VersionRange has the versions of one dependency.
type VersionRange struct {
	Name string
	// WorksFrom is the oldest version that is not unsupported.
	WorksFrom Minor
	// Tested is what CI and the acceptance runs cover, both ends included.
	Tested [2]Minor
	// Majors says that only major versions matter (PostgreSQL).
	Majors bool
}

// Fit is how a version fits the envelope.
type Fit string

// The fits. Untested is newer than tested, or between WorksFrom and the
// tested range.
const (
	Supported   Fit = "supported"
	Untested    Fit = "untested"
	Unsupported Fit = "unsupported"
)

// ParseFit reads a fit by its wire name.
func ParseFit(s string) (Fit, error) {
	switch f := Fit(s); f {
	case Supported, Untested, Unsupported:
		return f, nil
	}
	return "", kerrors.New(kerrors.Validation, "unknown fit `%s`", s)
}

func (f Fit) String() string { return string(f) }

// Fit says how version fits the range.
func (r VersionRange) Fit(version Minor) Fit {
	switch {
	case version.Compare(r.WorksFrom) < 0:
		return Unsupported
	case version.Compare(r.Tested[0]) >= 0 && version.Compare(r.Tested[1]) <= 0:
		return Supported
	default:
		return Untested
	}
}

func (r VersionRange) show(v Minor) string {
	if r.Majors {
		return strconv.FormatUint(uint64(v.Major), 10)
	}
	return v.String()
}

func (r VersionRange) json() map[string]any {
	return map[string]any{
		"name":      r.Name,
		"worksFrom": r.show(r.WorksFrom),
		"tested":    []any{r.show(r.Tested[0]), r.show(r.Tested[1])},
	}
}

// Describe is a line for `doctor`: how version fits, and why.
func (r VersionRange) Describe(version Minor) (Fit, string) {
	lo, hi := r.show(r.Tested[0]), r.show(r.Tested[1])
	fit := r.Fit(version)
	shown, worksFrom := r.show(version), r.show(r.WorksFrom)
	switch fit {
	case Supported:
		return fit, fmt.Sprintf("%s %s is supported (tested %s to %s)", r.Name, shown, lo, hi)
	case Untested:
		return fit, fmt.Sprintf(
			"%s %s is outside the tested range %s to %s; it may work, but it is not supported", r.Name, shown, lo, hi)
	case Unsupported:
		return fit, fmt.Sprintf(
			"%s %s is not supported: use %s or newer (tested %s to %s)", r.Name, shown, worksFrom, lo, hi)
	}
	return fit, ""
}

// Kubernetes is the range of Kubernetes versions.
func Kubernetes() VersionRange {
	return VersionRange{Name: "Kubernetes", WorksFrom: Minor{1, 29}, Tested: [2]Minor{{1, 31}, {1, 36}}}
}

// PostgreSQL is the range of PostgreSQL versions; only majors matter.
func PostgreSQL() VersionRange {
	return VersionRange{
		Name:      "PostgreSQL",
		WorksFrom: Minor{15, 0},
		Tested:    [2]Minor{{15, 0}, {18, math.MaxUint32}},
		Majors:    true,
	}
}

// GatewayAPI is the range of Gateway API versions.
func GatewayAPI() VersionRange {
	return VersionRange{Name: "Gateway API", WorksFrom: Minor{1, 1}, Tested: [2]Minor{{1, 2}, {1, 5}}}
}

// CertManager is the range of cert-manager versions.
func CertManager() VersionRange {
	return VersionRange{Name: "cert-manager", WorksFrom: Minor{1, 15}, Tested: [2]Minor{{1, 16}, {1, 21}}}
}

// Hosts are the host operating systems `kuben setup` supports.
func Hosts() []string {
	return []string{
		"Ubuntu 22.04, 24.04",
		"Debian 12, 13",
		"RHEL, Rocky and AlmaLinux 9, 10",
	}
}

// Architectures are the CPU architectures Kuben is built for.
func Architectures() []string { return []string{"amd64", "arm64"} }

// Limit is one claim of the Supported MVP about one area.
type Limit struct {
	Area  string
	Claim string
}

// Limits are what the Supported MVP claims, and what it does not.
func Limits() []Limit {
	return []Limit{
		{"clusters", "one cluster per installation"},
		{"placements", "one placement (namespace) per environment"},
		{"workloads", "stateless web and worker processes; production data in an external PostgreSQL"},
		{"availability", "single failure domain: no high-availability claim (M7)"},
		{"nodes", "node lifecycle (add, drain, replace) is the operator's (M6)"},
		{
			"previews",
			"one preview environment per GitHub pull request, with a lifetime; a fork's preview gets no secrets",
		},
		{"registries", "OCI registries with basic or token auth; GitHub for Git sources"},
		{"gateways", "one Gateway API implementation; Traefik (k3s) is the tested one"},
	}
}

// Envelope is the envelope as JSON, for bundles and the API. Marshalled, its
// keys are sorted, as serde_json sorted them.
func Envelope() map[string]any {
	all := Limits()
	limits := make([]any, 0, len(all))
	for _, l := range all {
		limits = append(limits, map[string]any{"area": l.Area, "claim": l.Claim})
	}
	return map[string]any{
		"profile":       "supported-mvp",
		"kubernetes":    Kubernetes().json(),
		"postgresql":    PostgreSQL().json(),
		"gatewayApi":    GatewayAPI().json(),
		"certManager":   CertManager().json(),
		"hosts":         anySlice(Hosts()),
		"architectures": anySlice(Architectures()),
		"limits":        limits,
	}
}

func anySlice(items []string) []any {
	out := make([]any, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}
