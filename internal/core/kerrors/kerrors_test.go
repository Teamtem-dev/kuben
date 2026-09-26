package kerrors_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
)

func TestMessagesAndCodes(t *testing.T) {
	cases := []struct {
		err  error
		code kerrors.Code
		text string
	}{
		{kerrors.New(kerrors.NotFound, "app `%s`", "web"), kerrors.NotFound, "not found: app `web`"},
		{kerrors.ErrForbidden, kerrors.Forbidden, "forbidden"},
		{kerrors.TooMany(30), kerrors.RateLimited, "too many attempts; retry in 30s"},
		{kerrors.Wrap(io.EOF, "read the plan"), kerrors.Internal, "internal error: read the plan: EOF"},
		{io.EOF, kerrors.Internal, "EOF"},
	}
	for _, c := range cases {
		if got := kerrors.CodeOf(c.err); got != c.code {
			t.Errorf("%v: code %s, want %s", c.err, got, c.code)
		}
		if c.err.Error() != c.text {
			t.Errorf("text %q, want %q", c.err.Error(), c.text)
		}
	}
}

func TestWrapOfNilIsStillAnError(t *testing.T) {
	err := kerrors.Wrap(nil, "waiting")
	if err.Error() != "internal error: waiting" || errors.Unwrap(err) != nil {
		t.Fatalf("got %v", err)
	}
}

func TestIsMatchesByCodeThroughWrapping(t *testing.T) {
	err := fmt.Errorf("load: %w", kerrors.New(kerrors.NotFound, "release 7"))
	if !errors.Is(err, kerrors.ErrNotFound) {
		t.Fatal("a wrapped not-found is a not-found")
	}
	if errors.Is(err, kerrors.ErrConflict) {
		t.Fatal("and not a conflict")
	}
	if !errors.Is(kerrors.Wrap(io.EOF, "x"), io.EOF) {
		t.Fatal("the cause stays reachable")
	}
}
