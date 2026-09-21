package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

type object = map[string]json.RawMessage

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// required decodes the member key, which must be there and not null.
func required[T any](o object, key string, out *T) error {
	raw, ok := o[key]
	if !ok || isNull(raw) {
		return fmt.Errorf("missing field `%s`", key)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("field `%s`: %w", key, err)
	}
	return nil
}

// optional decodes the member key; absent and null are both absent.
func optional[T any](o object, key string, out *opt.Val[T]) error {
	raw, ok := o[key]
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

// numericID reads an id GitHub sends as a string, or as a number.
func numericID(o object, key string, out *uint64) error {
	var text string
	if required(o, key, &text) != nil {
		return required(o, key, out)
	}
	// Rust's integer parser takes one leading `+`.
	id, err := strconv.ParseUint(strings.TrimPrefix(text, "+"), 10, 64)
	if err != nil {
		return fmt.Errorf("field `%s`: %q is not a numeric id", key, text)
	}
	*out = id
	return nil
}

// UnmarshalJSON reads the claims of a token; every claim without a default
// must be there.
func (c *GithubClaims) UnmarshalJSON(data []byte) error {
	var o object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var out GithubClaims
	text := []struct {
		key string
		out *string
	}{
		{"iss", &out.Iss}, {"sub", &out.Sub}, {"jti", &out.Jti},
		{"repository", &out.Repository}, {"repository_owner", &out.RepositoryOwner},
		{"ref", &out.GitRef}, {"event_name", &out.EventName},
	}
	for _, f := range text {
		if err := required(o, f.key, f.out); err != nil {
			return err
		}
	}
	maybe := []struct {
		key string
		out *opt.Val[string]
	}{
		{"environment", &out.Environment}, {"workflow_ref", &out.WorkflowRef},
		{"run_id", &out.RunID}, {"actor", &out.Actor},
	}
	for _, f := range maybe {
		if err := optional(o, f.key, f.out); err != nil {
			return err
		}
	}
	steps := []error{
		audiences(o, &out.Aud),
		required(o, "iat", &out.Iat),
		optional(o, "nbf", &out.Nbf),
		required(o, "exp", &out.Exp),
		numericID(o, "repository_id", &out.RepositoryID),
		numericID(o, "repository_owner_id", &out.RepositoryOwnerID),
	}
	for _, err := range steps {
		if err != nil {
			return err
		}
	}
	*c = out
	return nil
}

// MarshalJSON writes the policy as the API and the database have it: every
// member, and lists never null.
func (p TrustPolicy) MarshalJSON() ([]byte, error) {
	type wire TrustPolicy
	w := wire(p)
	for _, list := range []*[]string{&w.Refs, &w.Environments, &w.Events} {
		if *list == nil {
			*list = []string{}
		}
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads a policy. Only `environments` may be left out, and
// the role must be one.
func (p *TrustPolicy) UnmarshalJSON(data []byte) error {
	var o object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var (
		out  TrustPolicy
		role string
	)
	steps := []error{
		required(o, "repositoryId", &out.RepositoryID),
		required(o, "repositoryOwnerId", &out.RepositoryOwnerID),
		required(o, "refs", &out.Refs),
		required(o, "events", &out.Events),
		required(o, "role", &role),
		required(o, "tokenTtlSecs", &out.TokenTTLSecs),
	}
	for _, err := range steps {
		if err != nil {
			return err
		}
	}
	if raw, ok := o["environments"]; ok {
		if err := required(object{"environments": raw}, "environments", &out.Environments); err != nil {
			return err
		}
	}
	parsed, err := perm.ParseRole(role)
	if err != nil {
		return err
	}
	out.Role = parsed
	*p = out
	return nil
}
