package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// secretKeyring is the keyring of managed secrets (serve.rs secret_keyring,
// M4.4): read from secrets.keyring_file (default `secrets.keyring` in the
// state directory), created there with one fresh key on the first start,
// checked against the keys this installation has used, and with older
// revisions resealed under its current key. A keyring that is not the
// installation's, or a file anyone else may read, stops the server.
func secretKeyring(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) (*keyring.Keyring, error) {
	file := cfg.SecretKeyringFile()
	ring, err := keyring.LoadOrCreate(file)
	if err != nil {
		return nil, fmt.Errorf("secret keyring: %w", err)
	}
	if _, err := keyring.Prepare(ctx, st, ring, logger); err != nil {
		return nil, fmt.Errorf("secret keyring: %w", err)
	}
	logger.Info("secret keyring ready", "file", file, "key_version", ring.Current())
	return ring, nil
}
