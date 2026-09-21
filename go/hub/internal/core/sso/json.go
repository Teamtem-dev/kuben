package sso

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

type object = map[string]json.RawMessage

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// required takes the member key out of o; it must be there and not null.
func required[T any](o object, key string, out *T) error {
	raw, ok := o[key]
	if !ok || isNull(raw) {
		return fmt.Errorf("missing field `%s`", key)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("field `%s`: %w", key, err)
	}
	delete(o, key)
	return nil
}

// optional takes the member key out of o; absent and null are both absent.
func optional[T any](o object, key string, out *opt.Val[T]) error {
	raw, ok := o[key]
	delete(o, key)
	if !ok || isNull(raw) {
		*out = opt.None[T]()
		return nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("field `%s`: %w", key, err)
	}
	*out = opt.Some(v)
	return nil
}

// audiences reads `aud`: one string, or a list of them.
func audiences(o object, out *[]string) error {
	var one string
	if required(o, "aud", &one) == nil {
		*out = []string{one}
		return nil
	}
	return required(o, "aud", out)
}

// UnmarshalJSON reads the claims of an ID token: the ones Kuben names, and
// every other one into Other.
func (c *IDClaims) UnmarshalJSON(data []byte) error {
	var o object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var out IDClaims
	steps := []error{
		required(o, "iss", &out.Iss),
		audiences(o, &out.Aud),
		required(o, "sub", &out.Sub),
		required(o, "exp", &out.Exp),
		required(o, "iat", &out.Iat),
		optional(o, "nonce", &out.Nonce),
		optional(o, "email", &out.Email),
		optional(o, "email_verified", &out.EmailVerified),
		optional(o, "name", &out.Name),
	}
	for _, err := range steps {
		if err != nil {
			return err
		}
	}
	out.Other = make(map[string]any, len(o))
	for key, raw := range o {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("field `%s`: %w", key, err)
		}
		out.Other[key] = v
	}
	*c = out
	return nil
}
