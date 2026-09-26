package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/auth"
)

type authFixture struct {
	Passwords []struct {
		Password string `json:"password"`
		Phc      string `json:"phc"`
	} `json:"passwords"`
	Tokens []struct {
		Plaintext     string `json:"plaintext"`
		ID            string `json:"id"`
		Secret        string `json:"secret"`
		SHA256        string `json:"sha256"`
		DisplayPrefix string `json:"displayPrefix"`
	} `json:"tokens"`
	Session struct {
		Input  string `json:"input"`
		SHA256 string `json:"sha256"`
	} `json:"session"`
}

func loadFixture(t *testing.T) authFixture {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/compat/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	var f authFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRustPasswordHashesVerify(t *testing.T) {
	f := loadFixture(t)
	h := auth.InsecureForTests()
	for _, p := range f.Passwords {
		if !h.Verify(p.Password, p.Phc) {
			t.Errorf("a hash Rust wrote must verify: %s", p.Phc)
		}
		if h.Verify(p.Password+"x", p.Phc) {
			t.Errorf("a wrong password must not verify")
		}
	}
}

func TestGoHashesAreRustShaped(t *testing.T) {
	h := auth.NewHasher(19*1024, 2, 1)
	phc, err := h.Hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("got %s", phc)
	}
	parts := strings.Split(phc, "$")
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		t.Fatalf("salt %q: %v", parts[4], err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != 32 {
		t.Fatalf("key %q: %v", parts[5], err)
	}
	if !h.Verify("s3cret", phc) || h.Verify("wrong", phc) || h.Verify("s3cret", "not-a-phc-string") {
		t.Fatal("round trip")
	}
	if h.DummyHash() == "" || !strings.HasPrefix(h.DummyHash(), "$argon2id$") {
		t.Fatal("the dummy is a real hash")
	}
}

func TestRustTokensParseAndMatch(t *testing.T) {
	f := loadFixture(t)
	if len(f.Tokens) == 0 {
		t.Fatal("no tokens")
	}
	for _, tok := range f.Tokens {
		id, secret, ok := auth.ParseAPIToken(tok.Plaintext)
		if !ok || id.String() != tok.ID || secret != tok.Secret {
			t.Fatalf("%s: got %s %s %v", tok.Plaintext, id, secret, ok)
		}
		stored, err := base64.StdEncoding.DecodeString(tok.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if !auth.SecretMatches(secret, stored) || auth.SecretMatches(secret+"x", stored) {
			t.Fatal("secret comparison")
		}
		if got := auth.TokenDisplayPrefix(tok.Plaintext); got != tok.DisplayPrefix {
			t.Errorf("prefix %s, want %s", got, tok.DisplayPrefix)
		}
	}
	sum := base64.StdEncoding.EncodeToString(auth.SHA256([]byte(f.Session.Input)))
	if sum != f.Session.SHA256 {
		t.Fatalf("session hash %s, want %s", sum, f.Session.SHA256)
	}
}

func TestGoTokensRoundTripAndGarbageIsRefused(t *testing.T) {
	plaintext, id, hash := auth.NewAPIToken()
	parsed, secret, ok := auth.ParseAPIToken(plaintext)
	if !ok || parsed != id || !auth.SecretMatches(secret, hash) {
		t.Fatalf("round trip of %s", plaintext)
	}
	if !strings.HasPrefix(plaintext, auth.TokenDisplayPrefix(plaintext)) {
		t.Fatal("display prefix")
	}
	for _, bad := range []string{"", "kbn_pat_", "kbn_pat_xyz_abc", "Bearer x", strings.Replace(plaintext, "kbn_pat_", "kbn_xxx_", 1)} {
		if _, _, ok := auth.ParseAPIToken(bad); ok {
			t.Errorf("%q must be refused", bad)
		}
	}
	a, ha := auth.NewSessionID()
	b, hb := auth.NewSessionID()
	if a == b || string(ha) == string(hb) || len(ha) != 32 || string(auth.SHA256([]byte(a))) != string(ha) {
		t.Fatal("session ids")
	}
}

func TestCookies(t *testing.T) {
	cfg := config.Default()
	cfg.Server.PublicURL = opt.Some("https://kuben.example.com")
	c := auth.SessionCookie(cfg, "abc")
	if c.Name != auth.CookieNameSecure || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 12*3600 {
		t.Fatalf("got %+v", c)
	}
	plain := config.Default() // `auto` without an https public URL: the fresh http install
	c = auth.SessionCookie(plain, "abc")
	if c.Name != auth.CookieNameDev || c.Secure || !c.HttpOnly {
		t.Fatalf("got %+v", c)
	}
	if auth.RemovalCookie(plain).Name != auth.CookieNameDev || auth.RemovalCookie(plain).MaxAge >= 0 {
		t.Fatal("removal cookie")
	}
}
