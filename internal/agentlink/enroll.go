package agentlink

// The hub's enrollment service (enroll.rs, plan §11.4):
//
//  1. The agent generates its device key; the key never leaves the device,
//     and the digest of its public key is the device id.
//  2. With a bootstrap token it connects to the hub anonymously and sends
//     Enroll: the token, its cluster and a CSR signed by the device key,
//     which proves it holds the key.
//  3. The hub keeps only the token's hash. A token is bound to one cluster,
//     expires, and is redeemed once; redeeming it again with the same device
//     key resumes, so a lost answer never strands the agent, while any other
//     key is refused. Every refusal looks alike (invalidToken).
//  4. The hub issues a short-lived client certificate from the CSR's public
//     key alone. The agent then dials in with it (mTLS).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/Teamtem-dev/kuben/internal/agentlink/protocol"
)

// ResumeGrace is how long after its expiry a redeemed token can still be
// resumed by the device that redeemed it (an answer lost just before
// expiry).
const ResumeGrace = time.Hour

// NewToken is a new bootstrap token: shown to the operator once, stored as
// its hash.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("a bootstrap token: %w", err)
	}
	return "kbt_" + hex.EncodeToString(b[:]), nil
}

// TokenHash is the hash a token is stored and looked up by.
func TokenHash(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// Redeemed is how a token was redeemed.
type Redeemed int

// The redemptions.
const (
	// RedeemedFirst: for the first time.
	RedeemedFirst Redeemed = iota + 1
	// RedeemedResumed: again, by the device that redeemed it.
	RedeemedResumed
)

// TokenRefused is why a token was not redeemed; the agent only ever hears
// invalidToken.
type TokenRefused int

// The refusals.
const (
	// TokenUnknown: no such token.
	TokenUnknown TokenRefused = iota + 1
	// TokenExpired: the token has expired.
	TokenExpired
	// TokenOtherCluster: the token is for another cluster.
	TokenOtherCluster
	// TokenOtherDevice: the token was redeemed by another device.
	TokenOtherDevice
)

func (r TokenRefused) Error() string {
	switch r {
	case TokenUnknown:
		return "no such token"
	case TokenExpired:
		return "the token has expired"
	case TokenOtherCluster:
		return "the token is for another cluster"
	case TokenOtherDevice:
		return "the token was redeemed by another device"
	}
	return "no such token"
}

// TokenStore is where bootstrap tokens live (SQL in the hub, memory in
// tests).
type TokenStore interface {
	// Redeem redeems the token hashed as hash for device of cluster. The
	// error is a TokenRefused, or the store failing.
	Redeem(ctx context.Context, hash [32]byte, cluster, device string, now time.Time) (Redeemed, error)
}

// Enrollment is the hub's enrollment service.
type Enrollment struct {
	ca       ClusterCA
	tokens   TokenStore
	lifetime time.Duration
}

// NewEnrollment issues certificates from ca, valid for lifetime, against
// tokens.
func NewEnrollment(ca ClusterCA, tokens TokenStore, lifetime time.Duration) *Enrollment {
	return &Enrollment{ca: ca, tokens: tokens, lifetime: lifetime}
}

// CA is the CA that issues the certificates.
func (e *Enrollment) CA() ClusterCA { return e.ca }

// Enroll enrolls the device that signed csrPEM into cluster. A CSR that
// does not verify never consumes the token. The error is a
// protocol.Refusal.
func (e *Enrollment) Enroll(ctx context.Context, token, cluster, csrPEM string, now time.Time) (Issued, error) {
	csr, err := protocol.ParseCSR(csrPEM)
	if err != nil {
		return Issued{}, protocol.RefusalBadRequest
	}
	if _, err := e.tokens.Redeem(ctx, TokenHash(token), cluster, csr.DeviceID(), now); err != nil {
		return Issued{}, protocol.RefusalInvalidToken
	}
	issued, err := e.ca.Issue(cluster, csr, e.lifetime, now)
	if err != nil {
		return Issued{}, protocol.RefusalBadRequest
	}
	return issued, nil
}

// Renew renews the certificate of device in cluster, asked on the
// device's authenticated link: the CSR must be signed by the same device
// key. The error is a protocol.Refusal.
func (e *Enrollment) Renew(cluster, csrPEM, device string, now time.Time) (Issued, error) {
	csr, err := protocol.ParseCSR(csrPEM)
	if err != nil || csr.DeviceID() != device {
		return Issued{}, protocol.RefusalBadRequest
	}
	issued, err := e.ca.Issue(cluster, csr, e.lifetime, now)
	if err != nil {
		return Issued{}, protocol.RefusalBadRequest
	}
	return issued, nil
}

// ReceiveEnrollment is the hub's side, first half: read an anonymous
// peer's enrollment request and issue its certificate. A refusal is
// answered here; an issued certificate is not sent yet, so the hub records
// the device first and then hands it over ([AnswerEnrollment]): an agent
// with its certificate links at once, and the hub must already know its
// device. ok is false when nothing was issued.
func ReceiveEnrollment(ctx context.Context, stream io.ReadWriter, e *Enrollment, now time.Time) (Issued, bool, error) {
	m, open, err := protocol.ReadFrame(stream)
	if err != nil || !open {
		return Issued{}, false, err
	}
	answer := error(protocol.RefusalBadRequest)
	var issued Issued
	if enroll, isEnroll := m.(protocol.Enroll); isEnroll {
		issued, answer = e.Enroll(ctx, enroll.Token.Expose(), enroll.ClusterID, enroll.CSR, now)
	}
	if answer == nil {
		return issued, true, nil
	}
	reason := protocol.RefusalBadRequest
	message := "an anonymous connection may only enroll with a valid CSR"
	if answer == protocol.RefusalInvalidToken { //nolint:errorlint // Enroll returns the refusal itself
		reason, message = protocol.RefusalInvalidToken, "the bootstrap token is not valid for this cluster and device"
	}
	return Issued{}, false, protocol.WriteFrame(stream, protocol.Refused{Reason: reason, Message: message})
}

// AnswerEnrollment is the hub's side, second half: hand the agent its
// certificate.
func AnswerEnrollment(stream io.Writer, issued Issued) error {
	return protocol.WriteFrame(stream, protocol.Enrolled{
		Certificate: issued.CertificatePEM, NotAfter: issued.NotAfter.Unix(),
	})
}

// ServeEnrollment is both halves at once, for a hub that records nothing.
func ServeEnrollment(ctx context.Context, stream io.ReadWriter, e *Enrollment, now time.Time) (Issued, bool, error) {
	issued, ok, err := ReceiveEnrollment(ctx, stream, e, now)
	if err != nil || !ok {
		return Issued{}, false, err
	}
	if err := AnswerEnrollment(stream, issued); err != nil {
		return Issued{}, false, err
	}
	return issued, true, nil
}
