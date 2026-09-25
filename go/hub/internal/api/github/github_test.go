package github_test

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/github"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/source"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
)

// mirror is a stand-in for the RSA key: the "signature" is the message
// reversed.
func mirror(message []byte) ([]byte, error) {
	out := slices.Clone(message)
	slices.Reverse(out)
	return out, nil
}

func hmacHex(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSignaturesAreChecked(t *testing.T) {
	body := []byte(`{"zen":"hello"}`)
	good := "sha256=" + hmacHex([]byte("s3cret"), body)
	cases := []struct {
		name      string
		secret    string
		body      []byte
		signature string
		want      bool
	}{
		{"good", "s3cret", body, good, true},
		{"another secret", "other", body, good, false},
		{"another body", "s3cret", []byte("{}"), good, false},
		{"another algorithm", "s3cret", body, strings.Replace(good, "sha256=", "sha1=", 1), false},
		{"not hex", "s3cret", body, "sha256=zz", false},
		{"empty", "s3cret", body, "", false},
		{"hex case does not matter", "s3cret", body, "sha256=" + strings.ToUpper(hmacHex([]byte("s3cret"), body)), true},
		{"too long", "s3cret", body, good + "00", false},
		{"not a digit", "s3cret", body, good[:len(good)-1] + "g", false},
	}
	for _, c := range cases {
		if got := github.VerifySignature([]byte(c.secret), c.body, c.signature); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if !github.WebhookOnly([]byte("s3cret")).Verify(body, good) {
		t.Error("the App does not verify with its secret")
	}
}

// Rust read each pair with u8::from_str_radix, which also takes `+` and one
// digit; a signature written that way is accepted as it was.
func TestSignaturePairsReadAsRustReadThem(t *testing.T) {
	body := []byte("x")
	sig := hmacHex([]byte("k"), body)
	for i := 0; i < len(sig); i += 2 {
		if sig[i] == '0' {
			plus := "sha256=" + sig[:i] + "+" + sig[i+1:]
			if !github.VerifySignature([]byte("k"), body, plus) {
				t.Errorf("%s refused", plus)
			}
			minus := "sha256=" + sig[:i] + "-" + sig[i+1:]
			if github.VerifySignature([]byte("k"), body, minus) {
				t.Errorf("%s accepted", minus)
			}
			return
		}
	}
	t.Skip("no pair with a leading zero")
}

func decodeSegment(t *testing.T, part string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAppJWTsAreShortLivedAndNameTheApp(t *testing.T) {
	jwt, err := github.AppJWT(mirror, 12345, 1_700_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("%d parts", len(parts))
	}
	if diff := cmp.Diff(map[string]any{"alg": "RS256", "typ": "JWT"}, decodeSegment(t, parts[0])); diff != "" {
		t.Errorf("header (-want +got):\n%s", diff)
	}
	claims := decodeSegment(t, parts[1])
	if claims["iss"] != "12345" {
		t.Errorf("iss %v", claims["iss"])
	}
	iat, exp := claims["iat"].(float64), claims["exp"].(float64)
	if iat != 1_700_000_000-github.JWTBackdate {
		t.Errorf("iat %v", iat)
	}
	if exp-iat > 600 {
		t.Errorf("GitHub refuses JWTs longer than ten minutes: %v", exp-iat)
	}
	signed, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	want, _ := mirror([]byte(parts[0] + "." + parts[1]))
	if string(signed) != string(want) {
		t.Error("the signature is not over header.claims")
	}
	// The claims are serde_json's text: members in key order.
	rawClaims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if string(rawClaims) != `{"exp":1700000540,"iat":1699999940,"iss":"12345"}` {
		t.Errorf("claims %s", rawClaims)
	}
}

func TestKeysMustBeRSAPEM(t *testing.T) {
	if github.KeyFromPEM("not a key") == nil {
		t.Error("not a key accepted")
	}
	if github.KeyFromPEM("-----BEGIN RSA PRIVATE KEY-----\nAAAA") == nil {
		t.Error("unterminated block accepted")
	}
	garbage := "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"
	if github.KeyFromPEM(garbage) == nil {
		t.Error("garbage accepted")
	}
	der, pkcs8, err := github.PemDER("-----BEGIN PRIVATE KEY-----\nAQID\n-----END PRIVATE KEY-----")
	if err != nil || !pkcs8 || !slices.Equal(der, []byte{1, 2, 3}) {
		t.Errorf("%v %v %v", der, pkcs8, err)
	}
}

func TestKeysLoadAsPKCS1AndPKCS8(t *testing.T) {
	key := rsaKey(t, 2048)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"PKCS#1": pkcs1PEM(key),
		"PKCS#8": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
	} {
		if err := github.KeyFromPEM(text); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// ring signs with 2048 to 8192 bits only.
	if err := github.KeyFromPEM(pkcs1PEM(rsaKey(t, 1024))); err == nil || err.Error() != "TooSmall" {
		t.Errorf("a 1024-bit key: %v", err)
	}
	// A PKCS#8 key of another algorithm.
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edDER, err := x509.MarshalPKCS8PrivateKey(edKey)
	if err != nil {
		t.Fatal(err)
	}
	ed := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER}))
	if err := github.KeyFromPEM(ed); err == nil || err.Error() != "WrongAlgorithm" {
		t.Errorf("an Ed25519 key: %v", err)
	}
}

func TestAnIncompleteConfigurationIsRefused(t *testing.T) {
	_, err := github.New(config.DefaultGitCfg(), clock.System{})
	if !errors.As(err, new(github.NotConfigured)) {
		t.Errorf("default configuration: %v", err)
	}
	if want := "the GitHub App is not configured (git.github_app_id, git.github_private_key_file, git.github_webhook_secret)"; err == nil || err.Error() != want {
		t.Errorf("%v", err)
	}
	missing := config.DefaultGitCfg()
	missing.GithubAppID = opt.Some[uint64](1)
	missing.GithubPrivateKeyFile = opt.Some("/nonexistent/kuben-github.pem")
	missing.GithubWebhookSecret = "x"
	_, err = github.New(missing, clock.System{})
	var key github.KeyUnreadable
	if !errors.As(err, &key) || key.Path != "/nonexistent/kuben-github.pem" {
		t.Errorf("missing key file: %v", err)
	}
	if want := "cannot read the GitHub App key /nonexistent/kuben-github.pem: no such file or directory"; err == nil || err.Error() != want {
		t.Errorf("%v", err)
	}
}

func TestAnswersMapToProviderErrors(t *testing.T) {
	id, err := github.ParseRepository(http.StatusOK, []byte(`{"id":7}`), "r")
	if err != nil || id != 7 {
		t.Errorf("%d %v", id, err)
	}
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusNotFound, "", build.NotFound{What: "r"}},
		{http.StatusGone, "", build.NotFound{What: "r"}},
		{http.StatusUnprocessableEntity, "", build.NotFound{What: "r"}},
		{http.StatusForbidden, "", build.Refused{Reason: "r: HTTP 403 Forbidden"}},
		{http.StatusUnauthorized, "", build.Refused{Reason: "r: HTTP 401 Unauthorized"}},
		{http.StatusBadGateway, "", build.Unavailable{Reason: "r: HTTP 502 Bad Gateway"}},
		{599, "", build.Unavailable{Reason: "r: HTTP 599 <unknown status code>"}},
		{http.StatusOK, "{}", build.Unavailable{Reason: "r: unexpected answer: missing field `id`"}},
		{http.StatusOK, `{"id":null}`, build.Unavailable{Reason: "r: unexpected answer: missing field `id`"}},
	}
	for _, c := range cases {
		_, err := github.ParseRepository(c.status, []byte(c.body), "r")
		if !errors.Is(err, c.want) {
			t.Errorf("%d %s: %v, want %v", c.status, c.body, err, c.want)
		}
	}
	_, err = github.ParseRepository(http.StatusOK, []byte(`{"id":-1}`), "r")
	var unavailable build.Unavailable
	if !errors.As(err, &unavailable) {
		t.Errorf("a negative id: %v", err)
	}
}

func TestPathsAndURLsAreBuiltSafely(t *testing.T) {
	if got := github.Segment("feat/x y"); got != "feat%2Fx%20y" {
		t.Errorf("segment %q", got)
	}
	if got := github.Segment("a~b.c_d-e+f@g%"); got != "a~b.c_d-e%2Bf%40g%25" {
		t.Errorf("segment %q", got)
	}
	app := github.NewWithSigner(1, mirror, []byte("s"), config.DefaultGitCfg(), clock.System{})
	if got := app.CloneURL(repo(t, "acme/shop")); got != "https://github.com/acme/shop.git" {
		t.Errorf("clone URL %q", got)
	}
	cfg := config.DefaultGitCfg()
	cfg.GithubCloneURL = "https://git.example.com//"
	if got := github.NewWithSigner(1, mirror, nil, cfg, clock.System{}).CloneURL(repo(t, "acme/shop")); got != "https://git.example.com/acme/shop.git" {
		t.Errorf("clone URL %q", got)
	}
	now := time.Now().UnixMilli()
	if got := github.Expiry("2030-01-01T00:00:00Z", now); got != 1_893_456_000_000 {
		t.Errorf("expiry %d", got)
	}
	if got := github.Expiry("2030-01-01T00:00:00.9Z", now); got != 1_893_456_000_000 {
		t.Errorf("expiry %d (whole seconds)", got)
	}
	if got := github.Expiry("garbage", now); got <= now {
		t.Errorf("an unreadable expiry is not in the future: %d", got)
	}
	token := build.FetchToken{Token: "ghs_secret", ExpiresAt: 1_893_456_000_000}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", token, token, token), "ghs_secret") {
		t.Error("the token is printed")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", app, app), "s3cret") {
		t.Error("the App prints a secret")
	}
}

func repo(t *testing.T, name string) source.RepoName {
	t.Helper()
	r, err := source.ParseRepoName(name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func pkcs1PEM(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// keyFile writes key to a PEM file and returns its path.
func keyFile(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, []byte(pkcs1PEM(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
