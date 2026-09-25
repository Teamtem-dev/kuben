// Package auth has the credentials of the API: argon2id password hashes,
// session ids and API tokens (crates/kuben-api/src/auth/{password,session}.rs).
// What it writes must be readable by the Rust binary and the other way
// round, so both are pinned by fixtures the Rust code wrote
// (testdata/compat/auth.json).
package auth

import (
	"fmt"

	"github.com/alexedwards/argon2id"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
)

// Hasher hashes passwords with argon2id (v=19) into PHC strings (Invariant
// I-4). The parameters follow OWASP (m=19 MiB, t=2, p=1) and come from the
// configuration; the salt is 16 bytes and the key 32, as in Rust.
type Hasher struct {
	params *argon2id.Params
	dummy  string
}

// NewHasher is a hasher with the given memory (KiB), time and parallelism.
// Parameters argon2 refuses fall back to the OWASP defaults, as in Rust.
func NewHasher(memoryKiB, iterations uint32, parallelism uint8) *Hasher {
	p := &argon2id.Params{Memory: memoryKiB, Iterations: iterations, Parallelism: parallelism, SaltLength: 16, KeyLength: 32}
	if memoryKiB < 8*uint32(parallelism) || iterations < 1 || parallelism < 1 {
		p = &argon2id.Params{Memory: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	}
	h := &Hasher{params: p}
	h.dummy, _ = h.Hash("kuben-dummy-password-for-constant-time") //nolint:errcheck // an empty dummy only weakens timing equalisation
	return h
}

// HasherFromConfig is a hasher with the configured parameters.
func HasherFromConfig(cfg config.SecurityCfg) *Hasher {
	parallelism := cfg.Argon2P
	if parallelism > 255 {
		parallelism = 0 // refused: falls back to the defaults
	}
	return NewHasher(cfg.Argon2MKib, cfg.Argon2T, uint8(parallelism)) //nolint:gosec // bounded above
}

// InsecureForTests is a fast hasher for tests only.
func InsecureForTests() *Hasher { return NewHasher(8, 1, 1) }

// Hash is the PHC string of password.
func (h *Hasher) Hash(password string) (string, error) {
	phc, err := argon2id.CreateHash(password, h.params)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return phc, nil
}

// Verify reports whether password matches phc. A string that is not a PHC
// argon2id hash is a mismatch.
func (h *Hasher) Verify(password, phc string) bool {
	ok, err := argon2id.ComparePasswordAndHash(password, phc)
	return err == nil && ok
}

// DummyHash is a valid hash of an unknown password, verified when the
// account does not exist so that both cases take as long.
func (h *Hasher) DummyHash() string { return h.dummy }
