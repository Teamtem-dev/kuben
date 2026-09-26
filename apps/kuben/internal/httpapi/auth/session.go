package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
)

// Session cookie names: `__Host-` (Secure, no Domain, Path=/) when the
// cookie is secure, a plain name for http installs.
const (
	CookieNameSecure = "__Host-kuben_session"
	CookieNameDev    = "kuben_session"
)

// TokenPrefix starts every API token: `kbn_pat_<32 hex id>_<secret>`.
const TokenPrefix = "kbn_pat_"

// CookieName is the session cookie's name under cfg.
func CookieName(cfg config.Config) string {
	if cfg.CookieSecure() {
		return CookieNameSecure
	}
	return CookieNameDev
}

// SHA256 is the digest the store keeps instead of a session id or a token
// secret.
func SHA256(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// randomText is 32 random bytes in unpadded base64url.
func randomText() string {
	var b [32]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails (Go ≥ 1.24)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// NewSessionID is a fresh session id and its SHA-256: the id goes to the
// cookie only, the hash to the store.
func NewSessionID() (raw string, hash []byte) {
	raw = randomText()
	return raw, SHA256([]byte(raw))
}

// NewAPIToken is a fresh token: its plaintext (shown once), its id and the
// SHA-256 of its secret (stored).
func NewAPIToken() (plaintext string, id ids.TokenID, secretHash []byte) {
	id = ids.New[ids.Token]()
	secret := randomText()
	plaintext = TokenPrefix + strings.ReplaceAll(id.String(), "-", "") + "_" + secret
	return plaintext, id, SHA256([]byte(secret))
}

// ParseAPIToken splits `kbn_pat_<32 hex>_<secret>` into its id and secret.
// The id is fixed-length hex, so the first `_` after it separates them even
// when the secret holds `_`.
func ParseAPIToken(token string) (ids.TokenID, string, bool) {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok {
		return ids.TokenID{}, "", false
	}
	hexID, secret, ok := strings.Cut(rest, "_")
	if !ok || len(hexID) != 32 || secret == "" {
		return ids.TokenID{}, "", false
	}
	u, err := uuid.Parse(hexID)
	if err != nil {
		return ids.TokenID{}, "", false
	}
	return ids.From[ids.Token](u), secret, true
}

// TokenDisplayPrefix is the non-secret start of a token shown in lists, e.g.
// `kbn_pat_0192f3a1`.
func TokenDisplayPrefix(plaintext string) string {
	n := len(TokenPrefix) + 8
	runes := []rune(plaintext)
	if len(runes) < n {
		return plaintext
	}
	return string(runes[:n])
}

// SecretMatches compares a presented secret with a stored hash in constant
// time.
func SecretMatches(secret string, storedHash []byte) bool {
	return subtle.ConstantTimeCompare(SHA256([]byte(secret)), storedHash) == 1
}

// SessionCookie is the cookie carrying a new session id.
func SessionCookie(cfg config.Config, raw string) *http.Cookie {
	hours := cfg.Security.SessionTTLHours
	maxAge := int(min(hours, uint64(1<<31-1)/3600) * 3600) //nolint:gosec // bounded above
	return &http.Cookie{                                   //nolint:gosec // Secure only over HTTPS: plain HTTP on loopback is a supported setup (ADR-031)
		Name:     CookieName(cfg),
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		Secure:   cfg.CookieSecure(),
	}
}

// RemovalCookie clears the session cookie, as the Rust removal cookie did
// (same name and path, empty value, expired).
func RemovalCookie(cfg config.Config) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // Secure only over HTTPS, as the session cookie
		Name: CookieName(cfg), Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: cfg.CookieSecure(),
	}
}
