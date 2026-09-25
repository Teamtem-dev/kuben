// Package backup is `kuben backup` and `kuben restore` (M4.7; plan §17,
// S03), the port of crates/kuben/src/cli/backup.rs.
//
// A backup is a directory `kuben-<UTC time>` holding the database as a
// `pg_dump` custom archive, a manifest (versions, schema, checksum, the
// fingerprints of the secret keys) and, only when asked, the secret keyring.
// Kubernetes objects are not part of it: SQL is the only desired state, and
// a restore writes every app again.
//
// A restore needs an empty database. It checks the archive, loads it with
// `pg_restore`, migrates it forward, checks (or installs) the keyring, and
// then fences the time after the backup: sessions end, API tokens are
// revoked, every generation jumps ahead, and every app gets a run that
// writes it again. `pg_dump`/`pg_restore` come from PostgreSQL's client
// tools and must be at least as new as the server.
//
// The package imports neither internal/cli nor internal/serve: `kuben
// migrate` (ops/upgrade), `kuben backup` (cli) and the server call it.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	wire "github.com/Teamtem-dev/kuben/internal/jsonx"
	secrets "github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/version"
)

// Format is the manifest format this binary writes and reads.
const Format uint32 = 1

// The names inside a backup directory, and the prefix of its own name.
const (
	ManifestFile = "manifest.json"
	DumpFile     = "database.dump"
	KeyringFile  = "secrets.keyring"
	Prefix       = "kuben-"
)

// stampLayout is the UTC time in a backup's name (`%Y%m%dT%H%M%SZ`).
const stampLayout = "20060102T150405Z"

// detailChars is how much of a failure the backup record keeps.
const detailChars = 2048

// Manifest describes a backup. Its JSON members are in this order, as serde
// wrote the struct.
type Manifest struct {
	Format          uint32  `json:"format"`
	Kuben           string  `json:"kuben"`
	CreatedAt       int64   `json:"created_at"`
	Schema          int64   `json:"schema"`
	DatabaseVersion int32   `json:"database_version"`
	Dump            FileSum `json:"dump"`
	KeyringIncluded bool    `json:"keyring_included"`
	// KeyFingerprints is `version:sha256-hex` of every key the installation
	// used.
	KeyFingerprints []string `json:"key_fingerprints"`
}

// FileSum is a file of the backup with its size and checksum.
type FileSum struct {
	File   string `json:"file"`
	Bytes  uint64 `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// UnmarshalJSON requires every member, as serde did (unknown ones are
// ignored).
func (m *Manifest) UnmarshalJSON(data []byte) error {
	var o wire.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // the caller says what did not parse
	}
	var out Manifest
	err := errors.Join(
		wire.Required(o, "format", &out.Format),
		wire.Required(o, "kuben", &out.Kuben),
		wire.Required(o, "created_at", &out.CreatedAt),
		wire.Required(o, "schema", &out.Schema),
		wire.Required(o, "database_version", &out.DatabaseVersion),
		wire.Required(o, "dump", &out.Dump),
		wire.Required(o, "keyring_included", &out.KeyringIncluded),
		wire.Required(o, "key_fingerprints", &out.KeyFingerprints),
	)
	if err != nil {
		return err //nolint:wrapcheck // names the field
	}
	*m = out
	return nil
}

// UnmarshalJSON requires every member, as serde did.
func (f *FileSum) UnmarshalJSON(data []byte) error {
	var o wire.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // the caller says what did not parse
	}
	var out FileSum
	err := errors.Join(
		wire.Required(o, "file", &out.File),
		wire.Required(o, "bytes", &out.Bytes),
		wire.Required(o, "sha256", &out.SHA256),
	)
	if err != nil {
		return err //nolint:wrapcheck // names the field
	}
	*f = out
	return nil
}

// Options are the options of `kuben backup` (BackupOpts).
type Options struct {
	// Out is the directory the backup is written under; default
	// `backup.dir`.
	Out opt.Val[string]
	// IncludeKeyring also copies the secret keyring into the backup.
	IncludeKeyring bool
	// Keep is the number of backups kept under the directory; default
	// `backup.keep`.
	Keep opt.Val[uint32]
	// Scheduled records the backup as scheduled (the timer, the CronJob and
	// a migration set it).
	Scheduled bool
}

// joinPath is Rust's Path::join for a relative name: the directory as it
// was given, a separator only when it has none at its end. filepath.Join
// would clean `./x` to `x` and change the paths the command prints.
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

// SplitPassword is url without its password, and the password: the
// password travels to the PostgreSQL tools in their environment, never on
// a command line.
func SplitPassword(url string) (string, opt.Val[string]) {
	scheme, rest, ok := strings.Cut(url, "://")
	if !ok {
		return url, opt.None[string]()
	}
	authorityEnd := strings.IndexAny(rest, "/?")
	if authorityEnd < 0 {
		authorityEnd = len(rest)
	}
	authority, tail := rest[:authorityEnd], rest[authorityEnd:]
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return url, opt.None[string]()
	}
	userinfo, host := authority[:at], authority[at+1:]
	user, password, ok := strings.Cut(userinfo, ":")
	if !ok {
		return url, opt.None[string]()
	}
	return scheme + "://" + user + "@" + host + tail, opt.Some(PercentDecode(password))
}

// PercentDecode decodes `%XX` escapes; anything else, a broken escape
// included, stays as it is. Bytes that are not UTF-8 become U+FFFD.
func PercentDecode(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+3 <= len(s) {
			if b, ok := hexByte(s[i+1 : i+3]); ok {
				out = append(out, b)
				i += 3
				continue
			}
		}
		out = append(out, s[i])
		i++
	}
	return strings.ToValidUTF8(string(out), string(utf8.RuneError))
}

// hexByte is Rust's u8::from_str_radix(h, 16), which also takes a leading
// `+`.
func hexByte(h string) (byte, bool) {
	h = strings.TrimPrefix(h, "+")
	if h == "" || h[0] == '+' || h[0] == '-' {
		return 0, false
	}
	n, err := strconv.ParseUint(h, 16, 8)
	if err != nil {
		return 0, false
	}
	return byte(n), true
}

// pgTool is a PostgreSQL client tool for url, and url without its
// password.
func pgTool(ctx context.Context, tool, url string, args func(bare string) []string) *exec.Cmd {
	bare, password := SplitPassword(url)
	cmd := exec.CommandContext(ctx, tool, args(bare)...) //nolint:gosec // PostgreSQL's own tools, arguments without a shell
	cmd.Env = append(os.Environ(), "PGAPPNAME=kuben-"+tool)
	if p, ok := password.Get(); ok {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+p)
	}
	return cmd
}

// ClientToolsInstalled reports whether PostgreSQL's client tools are on the
// PATH.
func ClientToolsInstalled() bool {
	return exec.Command("pg_dump", "--version").Run() == nil //nolint:noctx // a version query, over at once
}

// toolMajor is the major version of tool, e.g. 17 for
// `pg_dump (PostgreSQL) 17.4`.
func toolMajor(ctx context.Context, tool string) (int32, error) {
	out, err := exec.CommandContext(ctx, tool, "--version").Output() //nolint:gosec // PostgreSQL's own tools
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return 0, fmt.Errorf("%s is not installed: install PostgreSQL's client tools: %w", tool, err)
	}
	major, ok := ParseMajor(strings.ToValidUTF8(string(out), string(utf8.RuneError)))
	if !ok {
		return 0, fmt.Errorf("unexpected `%s --version`", tool)
	}
	return major, nil
}

// ParseMajor is the first word of version that starts with a number (the
// part before its first dot).
func ParseMajor(version string) (int32, bool) {
	for _, word := range strings.Fields(version) {
		head, _, _ := strings.Cut(word, ".")
		if n, err := strconv.ParseInt(head, 10, 32); err == nil {
			return int32(n), true //nolint:gosec // parsed to 32 bits
		}
	}
	return 0, false
}

// execute runs cmd; a failure carries the tool's own complaint.
func execute(cmd *exec.Cmd, what string) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit):
		text := strings.ToValidUTF8(stderr.String(), string(utf8.RuneError))
		return fmt.Errorf("%s failed: %s", what, strings.TrimSpace(text))
	default:
		return fmt.Errorf("%s could not start: %w", what, err)
	}
}

// SHA256File is the size of the file at path and the lowercase hex SHA-256
// of its bytes.
func SHA256File(path string) (uint64, string, error) {
	f, err := os.Open(path) //nolint:gosec // a file of the backup
	if err != nil {
		return 0, "", err //nolint:wrapcheck // names the path
	}
	defer f.Close() //nolint:errcheck // read only
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", fmt.Errorf("reading %s: %w", path, err)
	}
	return uint64(n), hex.EncodeToString(h.Sum(nil)), nil //nolint:gosec // a byte count is not negative
}

// fingerprints is `version:hex` of every key of keyring, by version.
func fingerprints(keyring *secrets.Keyring) []string {
	fps := keyring.Fingerprints()
	out := make([]string, 0, len(fps))
	for _, f := range fps {
		out = append(out, strconv.FormatUint(uint64(f.Version), 10)+":"+hex.EncodeToString(f.SHA256[:]))
	}
	return out
}

// writePrivate writes a new file (mode 0600); an existing one is an error.
func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a file of the backup
	if err != nil {
		return err //nolint:wrapcheck // names the path
	}
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close() //nolint:wrapcheck // names the path
}

// isFile reports whether path is a regular file (following links), as
// Rust's Path::is_file.
func isFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return false
	}
	return info.Mode().IsRegular()
}

// Freshness says whether the newest good backup (finished at last) is fresh
// at nowMs, with maxAgeHours as the bound (0: not watched): the text says
// why, and it is the error when the backups are stale.
func Freshness(last opt.Val[int64], nowMs int64, maxAgeHours uint32) (string, error) {
	limit := int64(maxAgeHours) * 3_600_000
	at, ok := last.Get()
	switch {
	case !ok && maxAgeHours == 0:
		return "no backup yet (not watched: backup.max_age_hours = 0)", nil
	case !ok:
		return "", errors.New("no backup has been made: run `kuben backup` (or schedule it)")
	}
	age := clock.SaturatingSub(nowMs, at)
	hours := max(age, 0) / 3_600_000
	if maxAgeHours == 0 || age <= limit {
		return fmt.Sprintf("the newest good backup is %d h old", hours), nil
	}
	return "", fmt.Errorf("the newest good backup is %d h old, over backup.max_age_hours = %d", hours, maxAgeHours)
}

// Run is `kuben backup`, printing to the process's streams (Rust's
// backup::run, which `kuben migrate` calls with Scheduled set).
func Run(ctx context.Context, cfg config.Config, opts Options) error {
	return RunTo(ctx, cfg, opts, os.Stdout, os.Stderr)
}

// RunTo is [Run] printing to stdout and stderr.
func RunTo(ctx context.Context, cfg config.Config, opts Options, stdout, stderr io.Writer) error {
	c := clock.System{}
	root, ok := opts.Out.Get()
	if !ok {
		root = cfg.BackupDir()
	}
	startedAt := c.NowMs()
	st, err := store.ConnectUnmigrated(ctx, cfg.Database)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	defer st.Close()
	schema, err := st.SchemaVersion(ctx)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	dir, manifest, outcome := writeBackup(ctx, cfg, st, root, opts.IncludeKeyring, c)
	record := store.BackupRecord{
		Scheduled: opts.Scheduled, Succeeded: outcome == nil, Location: root,
		Kuben: version.Version, Schema: schema, StartedAt: startedAt,
	}
	if outcome == nil {
		record.Location = dir
		record.Bytes = opt.Some(manifest.Dump.Bytes)
		record.SHA256 = opt.Some(manifest.Dump.SHA256)
	} else {
		record.Detail = opt.Some(firstChars(outcome.Error(), detailChars))
	}
	record.FinishedAt = c.NowMs()
	if err := st.RecordBackup(ctx, record); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: the backup was not recorded in the database: %v\n", err) //nolint:errcheck // the terminal
	}
	if outcome != nil {
		return outcome
	}
	removed, err := prune(root, opts.Keep.Or(cfg.Backup.Keep))
	if err != nil {
		return err
	}
	return report(stdout, cfg, dir, manifest, removed)
}

// report prints what `kuben backup` wrote.
func report(w io.Writer, cfg config.Config, dir string, manifest Manifest, removed int) error {
	suffix := ""
	if removed > 0 {
		suffix = fmt.Sprintf("; %d older backup(s) removed", removed)
	}
	if _, err := fmt.Fprintf(w, "backup written to %s (%d MiB, schema %d, sha256 %s)%s\n",
		dir, manifest.Dump.Bytes>>20, manifest.Schema, manifest.Dump.SHA256, suffix); err != nil {
		return err //nolint:wrapcheck // stdout
	}
	var err error
	if manifest.KeyringIncluded {
		_, err = fmt.Fprintln(w, "the backup holds the secret keyring: whoever has it can read every secret; keep it encrypted")
	} else {
		_, err = fmt.Fprintf(w, "the secret keyring (%s) is not in the backup: back it up separately, a restore needs it\n",
			cfg.SecretKeyringFile())
	}
	return err //nolint:wrapcheck // stdout
}

// firstChars is the first n characters of s.
func firstChars(s string, n int) string {
	i := 0
	for at := range s {
		if i == n {
			return s[:at]
		}
		i++
	}
	return s
}

// writeBackup writes a backup under root and returns its directory and
// manifest.
func writeBackup(ctx context.Context, cfg config.Config, st *store.Store, root string, includeKeyring bool, c clock.Clock) (string, Manifest, error) {
	facts, err := st.DatabaseFacts(ctx)
	if err != nil {
		return "", Manifest{}, err //nolint:wrapcheck // explains itself
	}
	major, err := toolMajor(ctx, "pg_dump")
	if err != nil {
		return "", Manifest{}, err
	}
	if major < facts.Major() {
		return "", Manifest{}, fmt.Errorf("pg_dump %d is older than the PostgreSQL %d server; install client tools %d or newer",
			major, facts.Major(), facts.Major())
	}
	keyring, err := secrets.Load(cfg.SecretKeyringFile())
	if err != nil {
		return "", Manifest{}, fmt.Errorf("the secret keyring: %w", err)
	}
	stamp := time.UnixMilli(c.NowMs()).UTC().Format(stampLayout)
	dir := joinPath(root, Prefix+stamp)
	if err := os.MkdirAll(dir, 0o777); err != nil { //nolint:gosec // the umask applies; the directory itself is made 0700 below
		return "", Manifest{}, fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory permission 0700
		return "", Manifest{}, err //nolint:wrapcheck // names the path
	}
	dump := joinPath(dir, DumpFile)
	cmd := pgTool(ctx, "pg_dump", cfg.Database.URL.Expose(), func(bare string) []string {
		return []string{"--format=custom", "--no-owner", "--no-privileges", "--file", dump, "--dbname", bare}
	})
	if err := execute(cmd, "pg_dump"); err != nil {
		return "", Manifest{}, err
	}
	size, sum, err := SHA256File(dump)
	if err != nil {
		return "", Manifest{}, err
	}
	if includeKeyring {
		text, err := os.ReadFile(cfg.SecretKeyringFile())
		if err != nil {
			return "", Manifest{}, err //nolint:wrapcheck // names the path
		}
		if err := writePrivate(joinPath(dir, KeyringFile), text); err != nil {
			return "", Manifest{}, err
		}
	}
	schema, err := st.SchemaVersion(ctx)
	if err != nil {
		return "", Manifest{}, err //nolint:wrapcheck // explains itself
	}
	manifest := Manifest{
		Format: Format, Kuben: version.Version, CreatedAt: c.NowMs(), Schema: schema,
		DatabaseVersion: facts.VersionNum, Dump: FileSum{File: DumpFile, Bytes: size, SHA256: sum},
		KeyringIncluded: includeKeyring, KeyFingerprints: fingerprints(keyring),
	}
	text, err := EncodeManifest(manifest)
	if err != nil {
		return "", Manifest{}, err
	}
	if err := writePrivate(joinPath(dir, ManifestFile), text); err != nil {
		return "", Manifest{}, err
	}
	return dir, manifest, nil
}

// EncodeManifest is the manifest as serde_json::to_vec_pretty wrote it:
// two-space indents, no HTML escaping, no final newline.
func EncodeManifest(m Manifest) ([]byte, error) {
	if m.KeyFingerprints == nil {
		m.KeyFingerprints = []string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("encoding the manifest: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// prune removes all but the newest keep complete backups under root (0
// keeps everything) and returns how many it removed.
func prune(root string, keep uint32) (int, error) {
	if keep == 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err //nolint:wrapcheck // names the path
	}
	backups := []string{}
	for _, e := range entries {
		path := joinPath(root, e.Name())
		if strings.HasPrefix(e.Name(), Prefix) && isFile(joinPath(path, ManifestFile)) {
			backups = append(backups, path)
		}
	}
	// The names sort by time.
	slices.Sort(backups)
	excess := max(len(backups)-int(keep), 0)
	for _, old := range backups[:excess] {
		if err := os.RemoveAll(old); err != nil {
			return 0, fmt.Errorf("removing %s: %w", old, err)
		}
	}
	return excess, nil
}
