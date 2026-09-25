package backup

// `kuben restore` (backup.rs restore, checked_manifest, restore_keyring).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/version"
)

// RestoreOptions are the options of `kuben restore` (RestoreOpts).
type RestoreOptions struct {
	// From is a backup directory (`kuben-<time>`) made by `kuben backup`.
	From string
	// Check only checks that the backup is intact and restorable.
	Check bool
}

// CheckedManifest is the manifest of the backup in dir, checked against its
// dump.
func CheckedManifest(dir string) (Manifest, error) {
	text, err := os.ReadFile(joinPath(dir, ManifestFile))
	if err != nil {
		return Manifest{}, fmt.Errorf("%s is not a kuben backup (no %s): %w", dir, ManifestFile, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(text, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("the manifest does not parse: %w", err)
	}
	if manifest.Format != Format {
		return Manifest{}, fmt.Errorf("backup format %d is not supported by this kuben", manifest.Format)
	}
	if manifest.Dump.File != DumpFile {
		return Manifest{}, fmt.Errorf("unexpected dump file %s", manifest.Dump.File)
	}
	size, sum, err := SHA256File(joinPath(dir, DumpFile))
	if err != nil {
		return Manifest{}, err
	}
	if size != manifest.Dump.Bytes || sum != manifest.Dump.SHA256 {
		return Manifest{}, errors.New("the dump does not match its manifest: the backup is damaged")
	}
	return manifest, nil
}

// showTime is a unix time in milliseconds as jiff's Timestamp displayed it
// (RFC 3339 in UTC, fractional seconds only when there are some), `?`
// outside the years jiff can hold.
func showTime(ms int64) string {
	t := time.UnixMilli(ms).UTC()
	if t.Year() < -9999 || t.Year() > 9999 {
		return "?"
	}
	return t.Format(time.RFC3339Nano)
}

// Restore is `kuben restore`: it prints to stdout and logs the keyring's
// preparation to logger.
func Restore(ctx context.Context, cfg config.Config, opts RestoreOptions, stdout io.Writer, logger *slog.Logger) error {
	manifest, err := CheckedManifest(opts.From)
	if err != nil {
		return err
	}
	if manifest.Schema > store.LatestMigration() {
		return fmt.Errorf("the backup is from a newer Kuben (%s, schema %d); restore it with that version or newer",
			manifest.Kuben, manifest.Schema)
	}
	dump := joinPath(opts.From, DumpFile)
	list := pgTool(ctx, "pg_restore", "", func(string) []string { return []string{"--list", dump} })
	if err := execute(list, "pg_restore --list"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "backup of %s (Kuben %s, schema %d) is intact\n",
		showTime(manifest.CreatedAt), manifest.Kuben, manifest.Schema); err != nil {
		return err //nolint:wrapcheck // stdout
	}
	if opts.Check {
		return nil
	}
	keyring, err := restoreKeyring(cfg, opts.From, manifest, stdout)
	if err != nil {
		return err
	}
	url := strings.TrimSpace(cfg.Database.URL.Expose())
	empty, err := store.DatabaseIsEmpty(ctx, url)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	if !empty {
		return errors.New("the database is not empty: restore into a new, empty database")
	}
	cmd := pgTool(ctx, "pg_restore", url, func(bare string) []string {
		return []string{"--no-owner", "--no-privileges", "--exit-on-error", "--single-transaction", "--dbname", bare, dump}
	})
	if err := execute(cmd, "pg_restore"); err != nil {
		return err
	}
	return fence(ctx, cfg, manifest, keyring, stdout, logger)
}

// fence migrates the restored database, prepares the keyring and fences
// the time after the backup.
func fence(ctx context.Context, cfg config.Config, manifest Manifest, ring *keyring.Keyring, stdout io.Writer, logger *slog.Logger) error {
	st, err := store.Connect(ctx, cfg.Database)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	defer st.Close()
	if _, err := keyring.Prepare(ctx, st, ring, logger); err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	fenced, err := st.AfterRestore(ctx, manifest.CreatedAt, manifest.Kuben, version.Version)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	runs, err := st.RedeliverAll(ctx, "system:restore")
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	ageMin := max(clock.SaturatingSub(clock.System{}.NowMs(), manifest.CreatedAt), 0) / 60_000
	schema, err := st.SchemaVersion(ctx)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	if _, err := fmt.Fprintf(stdout,
		"restored: schema %d → %d; %d session(s) ended and %d API token(s) revoked (sign in again, issue new tokens); "+
			"%d app(s) raised to a new generation, %d run(s) started to write them again\n",
		manifest.Schema, schema, fenced.SessionsEnded, fenced.TokensRevoked, fenced.TargetsRaised, runs); err != nil {
		return err //nolint:wrapcheck // stdout
	}
	_, err = fmt.Fprintf(stdout,
		"data written in the %d minute(s) between the backup and now is not in the restored database\n", ageMin)
	return err //nolint:wrapcheck // stdout
}

// restoreKeyring is the keyring the restored secrets need: the configured
// one, else the one in the backup (installed in its place). Its keys must
// be the backup's.
func restoreKeyring(cfg config.Config, from string, manifest Manifest, stdout io.Writer) (*keyring.Keyring, error) {
	path := cfg.SecretKeyringFile()
	var ring *keyring.Keyring
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		k, err := keyring.Load(path)
		if err != nil {
			return nil, err //nolint:wrapcheck // names the path
		}
		ring = k
	case manifest.KeyringIncluded:
		text, err := os.ReadFile(joinPath(from, KeyringFile))
		if err != nil {
			return nil, fmt.Errorf("the keyring in the backup: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil { //nolint:gosec // the umask applies, as create_dir_all
			return nil, err //nolint:wrapcheck // names the path
		}
		k, err := keyring.Install(path, string(text))
		clear(text)
		if err != nil {
			return nil, err //nolint:wrapcheck // names the path
		}
		if _, err := fmt.Fprintf(stdout, "installed the backup's secret keyring at %s\n", path); err != nil {
			return nil, err //nolint:wrapcheck // stdout
		}
		ring = k
	default:
		return nil, fmt.Errorf("no secret keyring at %s and none in the backup: put the installation's keyring there first", path)
	}
	ours := fingerprints(ring)
	missing := 0
	for _, f := range manifest.KeyFingerprints {
		if !slices.Contains(ours, f) {
			missing++
		}
	}
	if missing > 0 {
		return nil, fmt.Errorf("the keyring at %s lacks keys the backup was sealed with (%d of %d): it is not this installation's",
			path, missing, len(manifest.KeyFingerprints))
	}
	return ring, nil
}
