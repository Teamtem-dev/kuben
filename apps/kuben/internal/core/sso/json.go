package sso

import (
	"encoding/json"
	"fmt"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
)

// audiences reads `aud`: one string, or a list of them.
func audiences(o jsonx.Object, out *[]string) error {
	var one string
	if jsonx.Take(o, "aud", &one) == nil {
		*out = []string{one}
		return nil
	}
	return jsonx.Take(o, "aud", out)
}

// UnmarshalJSON reads the claims of an ID token: the ones Kuben names, and
// every other one into Other.
func (c *IDClaims) UnmarshalJSON(data []byte) error {
	var o jsonx.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var out IDClaims
	steps := []error{
		jsonx.Take(o, "iss", &out.Iss),
		audiences(o, &out.Aud),
		jsonx.Take(o, "sub", &out.Sub),
		jsonx.Take(o, "exp", &out.Exp),
		jsonx.Take(o, "iat", &out.Iat),
		jsonx.TakeOptional(o, "nonce", &out.Nonce),
		jsonx.TakeOptional(o, "email", &out.Email),
		jsonx.TakeOptional(o, "email_verified", &out.EmailVerified),
		jsonx.TakeOptional(o, "name", &out.Name),
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
