// Package supportbundle is `kuben support-bundle` (M4.11; plan §19.3 S09), the
// port of crates/kuben/src/cli/support.rs: a local file for a support
// conversation.
//
// The bundle is one JSON file written with mode 0600 under
// `<state dir>/support`. Nothing is uploaded. What goes in is allowlisted:
//   - versions;
//   - the configuration keys in [ConfigAllowlist];
//   - the output of `kuben doctor`;
//   - database counts and error codes ([store.Store.SupportSummary]);
//   - the state of the cluster and of Kuben's own pods;
//   - and, only with `--logs`, the newest lines of Kuben's own logs.
//
// Every string passes [registry.RedactCredentials], and the file is
// bounded ([MaxBundleBytes]; logs are cut first). `--preview` shows what
// would go in and writes nothing. Older bundles beyond `--keep` are
// removed. Making a bundle is audited. Redaction is best effort: read the
// file before you share it.
package supportbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
)

// MaxBundleBytes is the largest bundle written.
const MaxBundleBytes = 8 << 20

// MaxLogLines is the log lines per container (or of the host service) at
// most.
const MaxLogLines = 5_000

// Prefix starts the name of every bundle file.
const Prefix = "kuben-support-"

// maxEvents is the warning events of Kuben's namespace at most.
const maxEvents = 200

// doctorTimeout bounds the `kuben doctor` a bundle runs.
const doctorTimeout = 2 * time.Minute

// stampLayout is the UTC time in a bundle's name (`%Y%m%dT%H%M%SZ`).
const stampLayout = "20060102T150405Z"

// ConfigAllowlist is the configuration a bundle may show: a key is kept
// when its dotted path is listed or lies under a listed path. Credentials,
// URLs with credentials and personal data are never listed.
func ConfigAllowlist() []string {
	return []string{
		"server.bind",
		"server.metrics_bind",
		"server.activator_bind",
		"server.public_url",
		"server.roles",
		"server.request_timeout_secs",
		"server.max_body_bytes",
		"database.max_connections",
		"runtime",
		"kube.watch_namespace",
		"kube.required",
		"kube.namespace",
		"kube.leader_election",
		"security.session_ttl_hours",
		"security.cookie_secure",
		"security.trust_forwarded_for",
		"security.insecure_setup",
		"security.password_min_length",
		"telemetry.log_format",
		"telemetry.log_level",
		"bootstrap.org_slug",
		"agent.bind",
		"agent.hub_name",
		"agent.certificate_hours",
		"agent.heartbeat_secs",
		"agent.local",
		"agent.namespace",
		"git.github_app_id",
		"git.github_api_url",
		"build.enabled",
		"build.namespace",
		"build.buildkit_image",
		"build.fetch_image",
		"build.railpack_image",
		"build.scanner_image",
		"build.deadline_secs",
		"build.max_concurrent",
		"build.max_concurrent_per_org",
		"build.insecure_registry",
		"build.rescan_hours",
		"ci.github_actions",
		"ci.github_oidc_issuer",
		"sso.enabled",
		"sso.issuer",
		"sso.scopes",
		"sso.group_claim",
		"sso.default_role",
		"sso.require_verified_email",
		"sso.disable_password_for_linked",
		"quota",
		"backup",
		"notify",
		"retention",
		"domains",
	}
}

// generic is v as the JSON value serde_json::to_value made of it: objects
// are map[string]any, arrays []any, numbers json.Number.
func generic(v any) (any, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding %T: %w", v, err)
	}
	return jsonx.DecodeAny(buf.Bytes()) //nolint:wrapcheck // explains itself
}

var marshalerType = reflect.TypeFor[json.Marshaler]() //nolint:gochecknoglobals // a type, never changed

// configValue is v as serde serialized the Rust configuration: a section is
// an object of its TOML keys (the koanf tags), every other value is its
// JSON (an optional one null when unset).
func configValue(v reflect.Value) (any, error) {
	t := v.Type()
	if v.Kind() != reflect.Struct || t.Implements(marshalerType) {
		return generic(v.Interface())
	}
	out := make(map[string]any, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("koanf"), ",")
		if !f.IsExported() || name == "" || name == "-" {
			continue
		}
		value, err := configValue(v.Field(i))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = value
	}
	return out, nil
}

// AllowedConfig is cfg as the bundle shows it: only [ConfigAllowlist]
// paths, with the dropped keys listed by name.
func AllowedConfig(cfg config.Config) map[string]any {
	full, err := configValue(reflect.ValueOf(cfg))
	if err != nil {
		full = nil
	}
	omitted := []string{}
	kept := keepAllowed(full, "", ConfigAllowlist(), &omitted)
	names := make([]any, 0, len(omitted))
	for _, n := range omitted {
		names = append(names, n)
	}
	return map[string]any{"values": kept, "omitted": names}
}

// keepAllowed is the allowlisted part of value (an object at path), in the
// order of its keys, as serde_json's sorted map.
func keepAllowed(value any, path string, allow []string, omitted *[]string) any {
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(m))
	for _, key := range slices.Sorted(maps.Keys(m)) {
		here := key
		if path != "" {
			here = path + "." + key
		}
		switch {
		case slices.ContainsFunc(allow, func(a string) bool { return a == here || strings.HasPrefix(here, a+".") }):
			out[key] = Redacted(m[key])
		case slices.ContainsFunc(allow, func(a string) bool { return strings.HasPrefix(a, here+".") }):
			out[key] = keepAllowed(m[key], here, allow, omitted)
		default:
			*omitted = append(*omitted, here)
		}
	}
	return out
}

// Redacted is value with every string passed through
// [registry.RedactCredentials].
func Redacted(value any) any {
	switch v := value.(type) {
	case string:
		return registry.RedactCredentials(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Redacted(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = Redacted(item)
		}
		return out
	default:
		return value
	}
}

// Pretty is v as serde_json::to_vec_pretty wrote a serde_json::Value: keys
// sorted, two-space indents, no final newline.
func Pretty(v any) ([]byte, error) {
	compact, err := jsonx.CanonicalValue(v)
	if err != nil {
		return nil, err //nolint:wrapcheck // explains itself
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(compact), "", "  "); err != nil {
		return nil, fmt.Errorf("indenting the bundle: %w", err)
	}
	return buf.Bytes(), nil
}

// Encode is sections as the bundle's bytes, with log lines cut (newest
// kept) until it fits in limit bytes. It cuts the lines in sections itself.
func Encode(sections map[string]any, limit int) ([]byte, error) {
	for {
		data, err := Pretty(sections)
		if err != nil {
			return nil, err
		}
		if len(data) <= limit {
			return data, nil
		}
		logs, ok := sections["logs"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the bundle takes %d bytes, more than %d, without logs to cut", len(data), limit)
		}
		// The longest list; of equally long ones the last, as Rust's max_by_key.
		longest, size := "", -1
		for _, key := range slices.Sorted(maps.Keys(logs)) {
			if lines, ok := logs[key].([]any); ok && len(lines) >= size {
				longest, size = key, len(lines)
			}
		}
		if size <= 0 {
			delete(sections, "logs")
			continue
		}
		lines, ok := logs[longest].([]any)
		if !ok {
			delete(sections, "logs")
			continue
		}
		logs[longest] = lines[(len(lines)+1)/2:]
	}
}

// joinPath is Rust's Path::join for a relative name (filepath.Join would
// clean the directory the command prints).
func joinPath(dir, name string) string {
	switch {
	case dir == "":
		return name
	case strings.HasSuffix(dir, "/"):
		return dir + name
	default:
		return dir + "/" + name
	}
}

// writeBundle writes data as a new bundle in dir (0700) with mode 0600, and
// returns its path and the hex SHA-256 of data.
func writeBundle(dir string, data []byte, now time.Time) (string, string, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil { //nolint:gosec // the umask applies; the directory is made 0700 below
		return "", "", fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory permission 0700
		return "", "", err //nolint:wrapcheck // names the path
	}
	path := joinPath(dir, Prefix+now.UTC().Format(stampLayout)+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the bundle
	if err != nil {
		return "", "", fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return "", "", errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return "", "", err //nolint:wrapcheck // names the path
	}
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:]), nil
}

// Prune removes all but the newest keep bundles in dir (one is always
// kept) and returns how many it removed.
func Prune(dir string, keep uint32) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err //nolint:wrapcheck // names the path
	}
	bundles := []string{}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" && strings.HasPrefix(e.Name(), Prefix) {
			bundles = append(bundles, joinPath(dir, e.Name()))
		}
	}
	// The names sort by time.
	slices.Sort(bundles)
	excess := max(len(bundles)-int(max(keep, 1)), 0)
	for _, old := range bundles[:excess] {
		if err := os.Remove(old); err != nil {
			return 0, fmt.Errorf("removing %s: %w", old, err)
		}
	}
	return excess, nil
}

// rustLines is Rust's str::lines: split at `\n`, a `\r` before it dropped,
// no empty last line after a final newline.
func rustLines(text string) []any {
	if text == "" {
		return []any{}
	}
	parts := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSuffix(p, "\r"))
	}
	return out
}
