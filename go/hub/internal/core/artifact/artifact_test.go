package artifact_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
)

const sha256 = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestAcceptsOnlyFullLowercaseDigests(t *testing.T) {
	got, err := artifact.ParseDigest(sha256)
	if err != nil || got.String() != sha256 || got.IsZero() {
		t.Fatalf("got %s, %v", got, err)
	}
	if _, err = artifact.ParseDigest("sha512:" + strings.Repeat("a", 128)); err != nil {
		t.Fatalf("sha512: %v", err)
	}
	for _, bad := range []string{
		"nginx:1.27",
		"sha256:abc",
		strings.ToUpper(sha256),
		sha256 + "0",
		strings.Replace(sha256, "sha256", "md5", 1),
		"sha512:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("g", 64),
		"sha256",
		"",
	} {
		got, err = artifact.ParseDigest(bad)
		if err == nil || !got.IsZero() {
			t.Errorf("%q is not a digest", bad)
		}
	}
}

func TestInvalidDigestsAreValidationErrors(t *testing.T) {
	_, err := artifact.ParseDigest("latest")
	var invalid *artifact.InvalidDigestError
	if !errors.As(err, &invalid) || invalid.Value != "latest" {
		t.Fatalf("got %v", err)
	}
	if err.Error() != `not an OCI sha256 or sha512 digest: "latest"` {
		t.Errorf("got %s", err)
	}
	if !errors.Is(err, kerr.ErrValidation) || kerr.CodeOf(err) != kerr.Validation {
		t.Errorf("got code %s", kerr.CodeOf(err))
	}
}

func TestJSONValidatesToo(t *testing.T) {
	var ok artifact.Digest
	if err := json.Unmarshal([]byte(`"`+sha256+`"`), &ok); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ok)
	if err != nil || string(raw) != `"`+sha256+`"` {
		t.Fatalf("got %s, %v", raw, err)
	}
	var bad artifact.Digest
	if err = json.Unmarshal([]byte(`"latest"`), &bad); err == nil || !bad.IsZero() {
		t.Fatalf("latest is not a digest: %v", err)
	}
}

func TestDigestsOrderAndKeyMaps(t *testing.T) {
	a, _ := artifact.ParseDigest(sha256)
	b, _ := artifact.ParseDigest("sha256:" + strings.Repeat("f", 64))
	if a.Compare(b) >= 0 || b.Compare(a) <= 0 || a.Compare(a) != 0 {
		t.Error("digests order by their text")
	}
	if seen := map[artifact.Digest]bool{a: true}; !seen[a] || seen[b] {
		t.Error("digests are map keys")
	}
}
