package sso

import (
	"encoding/json"
	"fmt"

	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

// audiences reads `aud`: one string, or a list of them.
func audiences(o wire.Object, out *[]string) error {
	var one string
	if wire.Take(o, "aud", &one) == nil {
		*out = []string{one}
		return nil
	}
	return wire.Take(o, "aud", out)
}

// UnmarshalJSON reads the claims of an ID token: the ones Kuben names, and
// every other one into Other.
func (c *IDClaims) UnmarshalJSON(data []byte) error {
	var o wire.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var out IDClaims
	steps := []error{
		wire.Take(o, "iss", &out.Iss),
		audiences(o, &out.Aud),
		wire.Take(o, "sub", &out.Sub),
		wire.Take(o, "exp", &out.Exp),
		wire.Take(o, "iat", &out.Iat),
		wire.TakeOptional(o, "nonce", &out.Nonce),
		wire.TakeOptional(o, "email", &out.Email),
		wire.TakeOptional(o, "email_verified", &out.EmailVerified),
		wire.TakeOptional(o, "name", &out.Name),
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
