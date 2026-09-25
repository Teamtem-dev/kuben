package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// Load reads the keyring at path, which must exist and be private.
func Load(path string) (*Keyring, error) {
	text, err := readText(path)
	if err != nil {
		return nil, &KeyringError{Path: path, Reason: err.Error()}
	}
	defer clear(text)
	return checkAndParse(path, text)
}

// Install writes text, a keyring, to path (mode 0600) when nothing is
// there, and returns it.
func Install(path, text string) (*Keyring, error) {
	k, reason := parse(text)
	if k == nil {
		return nil, &KeyringError{Path: path, Reason: reason}
	}
	if err := writePrivate(path, []byte(text)); err != nil {
		return nil, &KeyringError{Path: path, Reason: err.Error()}
	}
	return k, nil
}

// LoadOrCreate reads the keyring at path, creating it with one fresh key
// (mode 0600) when it does not exist.
func LoadOrCreate(path string) (*Keyring, error) {
	text, err := readText(path)
	switch {
	case err == nil:
		defer clear(text)
		return checkAndParse(path, text)
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, &KeyringError{Path: path, Reason: err.Error()}
	}
	var key [keyLen]byte
	defer clear(key[:])
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		return nil, ErrRandom
	}
	fresh := []byte("# Kuben secret keyring: version:base64 key. Back it up apart from the database.\n1:" +
		base64.StdEncoding.EncodeToString(key[:]) + "\n")
	defer clear(fresh)
	if err := writePrivate(path, fresh); err != nil {
		return nil, &KeyringError{Path: path, Reason: err.Error()}
	}
	return FromKeys(map[uint32][keyLen]byte{1: key}), nil
}

// readText is Rust's fs::read_to_string: the file must be UTF-8.
func readText(path string) ([]byte, error) {
	text, err := os.ReadFile(path) //nolint:gosec // G304: the configured keyring file
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if !utf8.Valid(text) {
		clear(text)
		return nil, errors.New("stream did not contain valid UTF-8")
	}
	return text, nil
}

func checkAndParse(path string, text []byte) (*Keyring, error) {
	if reason, ok := checkPrivate(path); !ok {
		return nil, &KeyringError{Path: path, Reason: reason}
	}
	k, reason := parse(string(text))
	if k == nil {
		return nil, &KeyringError{Path: path, Reason: reason}
	}
	return k, nil
}

// checkPrivate refuses a keyring anybody else may read or its group may
// write. Group read stays allowed: Kubernetes adds it to Secret volumes of
// pods with `fsGroup`.
func checkPrivate(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error(), false
	}
	mode := info.Mode().Perm()
	if mode&0o027 != 0 {
		return fmt.Sprintf("the keyring is readable by others or writable by its group (mode %o); chmod 600 it", mode), false
	}
	return "", true
}

// writePrivate creates path (mode 0600, never over an existing file) with
// bytes, and its directory when missing, and syncs it.
func writePrivate(path string, bytes []byte) error {
	// Rust's create_dir_all: 0777 less the umask.
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil { //nolint:gosec // G301: as Rust created it; the file itself is 0600
		return fmt.Errorf("create the keyring's directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the configured keyring file
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	_, werr := f.Write(bytes)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
