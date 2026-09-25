package ci

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/jsonx"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
)

// audiences reads `aud`: one string, or a list of them.
func audiences(o jsonx.Object, out *[]string) error {
	var one string
	if jsonx.Required(o, "aud", &one) == nil {
		*out = []string{one}
		return nil
	}
	return jsonx.Required(o, "aud", out)
}

// numericID reads an id GitHub sends as a string, or as a number.
func numericID(o jsonx.Object, key string, out *uint64) error {
	var text string
	if jsonx.Required(o, key, &text) != nil {
		return jsonx.Required(o, key, out)
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
	var o jsonx.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var out GithubClaims
	text := []struct {
		key string
		out *string
	}{
		{"iss", &out.Iss},
		{"sub", &out.Sub},
		{"jti", &out.Jti},
		{"repository", &out.Repository},
		{"repository_owner", &out.RepositoryOwner},
		{"ref", &out.GitRef},
		{"event_name", &out.EventName},
	}
	for _, f := range text {
		if err := jsonx.Required(o, f.key, f.out); err != nil {
			return err
		}
	}
	maybe := []struct {
		key string
		out *opt.Val[string]
	}{
		{"environment", &out.Environment},
		{"workflow_ref", &out.WorkflowRef},
		{"run_id", &out.RunID},
		{"actor", &out.Actor},
	}
	for _, f := range maybe {
		if err := jsonx.Optional(o, f.key, f.out); err != nil {
			return err
		}
	}
	steps := []error{
		audiences(o, &out.Aud),
		jsonx.Required(o, "iat", &out.Iat),
		jsonx.Optional(o, "nbf", &out.Nbf),
		jsonx.Required(o, "exp", &out.Exp),
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
	var o jsonx.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var (
		out  TrustPolicy
		role string
	)
	steps := []error{
		jsonx.Required(o, "repositoryId", &out.RepositoryID),
		jsonx.Required(o, "repositoryOwnerId", &out.RepositoryOwnerID),
		jsonx.Required(o, "refs", &out.Refs),
		jsonx.Required(o, "events", &out.Events),
		jsonx.Required(o, "role", &role),
		jsonx.Required(o, "tokenTtlSecs", &out.TokenTTLSecs),
	}
	for _, err := range steps {
		if err != nil {
			return err
		}
	}
	if raw, ok := o["environments"]; ok {
		if err := jsonx.Required(jsonx.Object{"environments": raw}, "environments", &out.Environments); err != nil {
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
