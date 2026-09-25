package github

import (
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
)

// Constants the tests check against.
const (
	JWTBackdate = jwtBackdate
	MaxBody     = maxBody
)

// AppJWT exposes appJWT with a stand-in signer.
func AppJWT(sign func([]byte) ([]byte, error), appID uint64, nowMs int64) (string, error) {
	return appJWT(sign, appID, nowMs)
}

// PemDER exposes pemDER.
func PemDER(text string) ([]byte, bool, error) { return pemDER(text) }

// KeyFromPEM exposes keyFromPEM (the key itself is not needed).
func KeyFromPEM(text string) error {
	_, err := keyFromPEM(text)
	return err
}

// Segment exposes segment.
func Segment(value string) string { return segment(value) }

// Expiry exposes expiry.
func Expiry(value string, nowMs int64) int64 { return expiry(value, nowMs) }

// ParseRepository exposes parse for a repository answer.
func ParseRepository(status int, body []byte, what string) (uint64, error) {
	answer, err := parse[repositoryBody](status, body, what)
	return answer.ID.Or(0), err
}

// NewWithSigner is an App that signs with sign (a stand-in for the RSA key).
func NewWithSigner(appID uint64, sign func([]byte) ([]byte, error), secret []byte, cfg config.GitCfg, clk clock.Clock) *App {
	return newApp(appID, sign, secret, cfg, clk)
}
