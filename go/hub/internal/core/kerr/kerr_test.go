package kerr_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
)

func TestMessagesAndCodes(t *testing.T) {
	cases := []struct {
		err  error
		code kerr.Code
		text string
	}{
		{kerr.New(kerr.NotFound, "app `%s`", "web"), kerr.NotFound, "not found: app `web`"},
		{kerr.ErrForbidden, kerr.Forbidden, "forbidden"},
		{kerr.TooMany(30), kerr.RateLimited, "too many attempts; retry in 30s"},
		{kerr.Wrap(io.EOF, "read the plan"), kerr.Internal, "internal error: read the plan: EOF"},
		{io.EOF, kerr.Internal, "EOF"},
	}
	for _, c := range cases {
		if got := kerr.CodeOf(c.err); got != c.code {
			t.Errorf("%v: code %s, want %s", c.err, got, c.code)
		}
		if c.err.Error() != c.text {
			t.Errorf("text %q, want %q", c.err.Error(), c.text)
		}
	}
}

func TestWrapOfNilIsStillAnError(t *testing.T) {
	err := kerr.Wrap(nil, "waiting")
	if err.Error() != "internal error: waiting" || errors.Unwrap(err) != nil {
		t.Fatalf("got %v", err)
	}
}

func TestIsMatchesByCodeThroughWrapping(t *testing.T) {
	err := fmt.Errorf("load: %w", kerr.New(kerr.NotFound, "release 7"))
	if !errors.Is(err, kerr.ErrNotFound) {
		t.Fatal("a wrapped not-found is a not-found")
	}
	if errors.Is(err, kerr.ErrConflict) {
		t.Fatal("and not a conflict")
	}
	if !errors.Is(kerr.Wrap(io.EOF, "x"), io.EOF) {
		t.Fatal("the cause stays reachable")
	}
}
