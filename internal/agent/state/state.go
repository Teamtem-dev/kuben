// Package state is the agent's state on disk and how it comes by an
// identity (crates/kuben-agent/src/state.rs).
//
//   - `device.key`: the device key, made on the first start and never sent
//     anywhere;
//   - `agent.crt`: the certificate the hub issued for that key.
//
// Both are readable by the agent's user alone, in a directory only it may
// enter. The bootstrap token is read from a file or stdin, and only when
// the agent has to enroll; it is never stored.
package state

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/internal/agent/link"
	"github.com/Teamtem-dev/kuben/internal/agentlink/protocol"
)

// The files of the state directory.
const (
	DeviceKey   = "device.key"
	Certificate = "agent.crt"
)

// IOError is a file of the state that cannot be read or written.
type IOError struct {
	Path string
	Err  error
}

func (e IOError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }

// Unwrap is the file system's error.
func (e IOError) Unwrap() error { return e.Err }

// InvalidError is a file of the state that holds something else than it
// should.
type InvalidError struct {
	Path   string
	Reason string
}

func (e InvalidError) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Reason) }

// ConnectError is the hub out of reach while enrolling.
type ConnectError struct{ Err error }

func (e ConnectError) Error() string { return fmt.Sprintf("cannot reach the hub to enroll: %v", e.Err) }

// Unwrap is the network's error.
func (e ConnectError) Unwrap() error { return e.Err }

// HandshakeError is TLS with the hub failing while enrolling.
type HandshakeError struct{ Err error }

func (e HandshakeError) Error() string {
	return fmt.Sprintf("TLS with the hub failed while enrolling: %v", e.Err)
}

// Unwrap is crypto/tls' error.
func (e HandshakeError) Unwrap() error { return e.Err }

// ErrNeedsEnrollment is an agent without a valid certificate and without a
// bootstrap token.
var ErrNeedsEnrollment = errors.New( //nolint:gochecknoglobals // a sentinel error
	"this agent has no valid certificate: enroll with a bootstrap token (--token-file or --token-stdin)")

// writeOwnerOnly writes content to path as a new file readable by its
// owner only; an existing file is replaced, so a leftover never keeps a
// wider mode.
func writeOwnerOnly(path string, content []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return IOError{Path: path, Err: err}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the agent's own path
	if err != nil {
		return IOError{Path: path, Err: err}
	}
	_, werr := f.Write(content)
	if err := errors.Join(werr, f.Close()); err != nil {
		return IOError{Path: path, Err: err}
	}
	return nil
}

// WritePrivate is writeOwnerOnly for the bootstrap package.
func WritePrivate(path string, content []byte) error { return writeOwnerOnly(path, content) }

// StoredCertificate is a certificate the agent keeps.
type StoredCertificate struct {
	PEM       string
	NotBefore time.Time
	NotAfter  time.Time
	// PublicKeyInfo is the SubjectPublicKeyInfo the certificate is for.
	PublicKeyInfo []byte
}

// ParseStoredCertificate reads the validity and key of a certificate in
// PEM.
func ParseStoredCertificate(text string) (StoredCertificate, error) {
	cert, err := protocol.ParseCertificatePEM(text)
	if err != nil {
		return StoredCertificate{}, err //nolint:wrapcheck // says what failed
	}
	return StoredCertificate{
		PEM: text, NotBefore: cert.NotBefore, NotAfter: cert.NotAfter, PublicKeyInfo: cert.RawSubjectPublicKeyInfo,
	}, nil
}

// State is the agent's state directory.
type State struct {
	dir string
}

// Open opens dir, creating it (entered by its owner only) if needed.
func Open(dir string) (State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return State{}, IOError{Path: dir, Err: err}
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory only its owner enters
		return State{}, IOError{Path: dir, Err: err}
	}
	return State{dir: dir}, nil
}

// Dir is the state directory.
func (s State) Dir() string { return s.dir }

func (s State) path(name string) string { return filepath.Join(s.dir, name) }

// DeviceKey is the device key, made and stored on the first call.
func (s State) DeviceKey() (protocol.DeviceKey, error) {
	path := s.path(DeviceKey)
	text, err := os.ReadFile(path) //nolint:gosec // the agent's own path
	switch {
	case err == nil:
		key, err := protocol.ParseDeviceKey(string(text))
		if err != nil {
			return protocol.DeviceKey{}, InvalidError{Path: path, Reason: err.Error()}
		}
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return protocol.DeviceKey{}, IOError{Path: path, Err: err}
	}
	key, err := protocol.GenerateDeviceKey()
	if err != nil {
		return protocol.DeviceKey{}, err //nolint:wrapcheck // says what failed
	}
	pem, err := key.PEM()
	if err != nil {
		return protocol.DeviceKey{}, err //nolint:wrapcheck // says what failed
	}
	return key, writeOwnerOnly(path, []byte(pem))
}

// Certificate is the stored certificate, if any.
func (s State) Certificate() (StoredCertificate, bool, error) {
	path := s.path(Certificate)
	text, err := os.ReadFile(path) //nolint:gosec // the agent's own path
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return StoredCertificate{}, false, nil
	case err != nil:
		return StoredCertificate{}, false, IOError{Path: path, Err: err}
	}
	stored, err := ParseStoredCertificate(string(text))
	if err != nil {
		return StoredCertificate{}, false, InvalidError{Path: path, Reason: err.Error()}
	}
	return stored, true, nil
}

// SaveCertificate keeps the certificate (PEM).
func (s State) SaveCertificate(pem string) error {
	return writeOwnerOnly(s.path(Certificate), []byte(pem))
}

// PinnedCA is the CA certificates in the PEM file at path: the hub CA to
// pin.
func PinnedCA(path string) ([]*x509.Certificate, error) {
	text, err := os.ReadFile(path) //nolint:gosec // a path the operator gave
	if err != nil {
		return nil, IOError{Path: path, Err: err}
	}
	cas, err := protocol.ParseCertificatesPEM(text)
	if err != nil {
		return nil, InvalidError{Path: path, Reason: err.Error()}
	}
	if len(cas) == 0 {
		return nil, InvalidError{Path: path, Reason: "no certificate in the file"}
	}
	return cas, nil
}

// TokenSource is where the bootstrap token comes from.
//
//sumtype:decl
type TokenSource interface{ tokenSource() }

// NoToken is no token at all.
type NoToken struct{}

// TokenFile is a file holding the token.
type TokenFile struct{ Path string }

// TokenStdin is the token on stdin.
type TokenStdin struct{}

func (NoToken) tokenSource()    {}
func (TokenFile) tokenSource()  {}
func (TokenStdin) tokenSource() {}

// trimmed is the token of text, surrounding whitespace removed; false when
// nothing is left.
func trimmed(text []byte) (string, bool) {
	token := strings.TrimSpace(string(text))
	return token, token != ""
}

// ReadToken is the bootstrap token from source, surrounding whitespace
// removed; false when there is none.
func ReadToken(source TokenSource, stdin io.Reader) (string, bool, error) {
	switch s := source.(type) {
	case NoToken:
		return "", false, nil
	case TokenFile:
		text, err := os.ReadFile(s.Path)
		if err != nil {
			return "", false, IOError{Path: s.Path, Err: err}
		}
		token, ok := trimmed(text)
		return token, ok, nil
	case TokenStdin:
		text, err := io.ReadAll(stdin)
		if err != nil {
			return "", false, IOError{Path: "<stdin>", Err: err}
		}
		token, ok := trimmed(text)
		return token, ok, nil
	}
	return "", false, nil
}

// HubAddress is how the agent reaches the hub to enroll.
type HubAddress struct {
	Connector link.Connector
	// Pinned is the hub CA the agent pins.
	Pinned []*x509.Certificate
	// Name is the name the hub's certificate carries.
	Name   string
	Logger *slog.Logger
}

// EnsureIdentity is the agent's identity: the stored certificate while it
// is valid at now and made for key; else a fresh enrollment with the token
// token yields (read only then), stored for the next start.
func EnsureIdentity(ctx context.Context, s State, key protocol.DeviceKey, hub HubAddress, clusterID string,
	token func() (string, bool, error), now time.Time,
) (tls.Certificate, error) {
	stored, found, err := s.Certificate()
	if err != nil {
		return tls.Certificate{}, err
	}
	if found {
		if stored.NotAfter.After(now) && bytes.Equal(stored.PublicKeyInfo, key.PublicKeyInfo()) {
			return key.Identity(stored.PEM) //nolint:wrapcheck // says what failed
		}
		hub.Logger.Info("the stored certificate is expired or for another key", "not_after", stored.NotAfter)
	}
	secret, ok, err := token()
	switch {
	case err != nil:
		return tls.Certificate{}, err
	case !ok:
		return tls.Certificate{}, ErrNeedsEnrollment
	}
	cfg, err := protocol.AgentConfig(hub.Pinned, hub.Name, nil)
	if err != nil {
		return tls.Certificate{}, err //nolint:wrapcheck // says what failed
	}
	raw, err := hub.Connector.Connect(ctx)
	if err != nil {
		return tls.Certificate{}, ConnectError{Err: err}
	}
	conn := tls.Client(raw, cfg)
	defer conn.Close() //nolint:errcheck // enrolled or not, the connection is done
	if err := conn.HandshakeContext(ctx); err != nil {
		return tls.Certificate{}, HandshakeError{Err: err}
	}
	enrolled, err := protocol.RequestEnrollment(conn, secret, clusterID, key)
	if err != nil {
		return tls.Certificate{}, err //nolint:wrapcheck // says what failed
	}
	if err := s.SaveCertificate(enrolled.Certificate); err != nil {
		return tls.Certificate{}, err
	}
	hub.Logger.Info("enrolled", "device", key.DeviceID(), "cluster", clusterID)
	return key.Identity(enrolled.Certificate) //nolint:wrapcheck // says what failed
}
