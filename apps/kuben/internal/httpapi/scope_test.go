package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// routes/scope.rs short_names.
func TestEnvironmentShortNames(t *testing.T) {
	cases := [][3]string{
		{"shop", "shop-prod", "prod"},
		{"shop", "legacy", "legacy"},
		{"shop", "shop-", "shop-"},
	}
	for _, c := range cases {
		if got := httpapi.EnvironmentShortName(c[0], c[1]); got != c[2] {
			t.Errorf("%s in %s: %s, want %s", c[1], c[0], got, c[2])
		}
	}
}

// The password every seeded account signs in with (tests/http.rs).
const seedPassword = "hunter22"

// fixture is tests/http.rs setup + seed_sql: organization acme with alice
// (owner) and bob (viewer), acme's project shop with environment prod, and
// a project of another organization.
type fixture struct {
	t     *testing.T
	c     *client
	store *store.Store
	org   ids.OrgID
	// projections play the cluster.
	projections *projection.Projections
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	return newFixtureWith(t, nil)
}

// newFixtureWith is newFixture on a configuration edit changed.
func newFixtureWith(t *testing.T, edit func(*config.Config)) fixture {
	t.Helper()
	f := newUsersFixture(t, edit)
	ctx := t.Context()
	st := f.store
	tenant, err := st.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	project, err := tenant.CreateProject(ctx, "shop", "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.CreateEnvironment(ctx, project, "prod", "prod", false); err != nil {
		t.Fatal(err)
	}
	if err := tenant.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateOrg(ctx, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err = st.Tenant(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.CreateProject(ctx, "secret-project", "Other tenant"); err != nil {
		t.Fatal(err)
	}
	if err := tenant.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

// newUsersFixture is tests/http.rs setup_with: the organization and its
// people (alice an owner, bob a viewer), nothing else.
func newUsersFixture(t *testing.T, edit func(*config.Config)) fixture {
	t.Helper()
	c, st, p := newServerWithProjections(t, edit)
	org, err := st.CreateOrg(t.Context(), "acme", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{t: t, c: c, store: st, org: org.ID, projections: p}
	// As in Rust's setup, alice and bob hold org roles without a membership row.
	f.user("alice@example.com", opt.Some("Alice"), perm.Owner, false)
	f.user("bob@example.com", opt.None[string](), perm.Viewer, false)
	return f
}

// user creates an account with role in the fixture's organization.
func (f fixture) user(email string, displayName opt.Val[string], role perm.Role, member bool) string {
	f.t.Helper()
	ctx := f.t.Context()
	hash, err := auth.InsecureForTests().Hash(seedPassword)
	if err != nil {
		f.t.Fatal(err)
	}
	u, err := f.store.CreateUser(ctx, email, displayName, opt.Some(hash))
	if err != nil {
		f.t.Fatal(err)
	}
	if member {
		if err := f.store.AddMembership(ctx, f.org, u.ID); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.store.BindOrgRole(ctx, f.org, u.ID, role); err != nil {
		f.t.Fatal(err)
	}
	return u.ID.String()
}

// member is tests/http.rs member: a member with role, signed in, and their
// id.
func (f fixture) member(email string, role perm.Role) (*client, string) {
	f.t.Helper()
	id := f.user(email, opt.None[string](), role, true)
	return f.signIn(email, seedPassword), id
}

// browser is a client of the same server with a cookie jar of its own.
func (f fixture) browser() *client {
	f.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return &client{t: f.t, base: f.c.base, http: &http.Client{Jar: jar}}
}

// signIn is a browser signed in as email.
func (f fixture) signIn(email, password string) *client {
	f.t.Helper()
	b, status, body := f.trySignIn(email, password)
	if status != 200 {
		f.t.Fatalf("sign in %s: %d %v", email, status, body)
	}
	return b
}

func (f fixture) trySignIn(email, password string) (*client, int, map[string]any) {
	f.t.Helper()
	b := f.browser()
	status, body, _ := b.do("POST", "/api/v1/auth/login", map[string]any{"email": email, "password": password})
	return b, status, body
}

// bearer calls the API with an API token and no cookie.
func (f fixture) bearer(token, method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	status, out, _ := f.browser().do(method, path, body, "Authorization", "Bearer "+token)
	return status, out
}

// list GETs a JSON array.
func (c *client) list(path string, headers ...string) (int, []map[string]any) {
	c.t.Helper()
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set(httpx.ClientHeader, "console")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp := c.send(req)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	var out []map[string]any
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("GET %s: %v in %s", path, err, strings.TrimSpace(string(raw)))
		}
	}
	return resp.StatusCode, out
}

// status is the status of a request.
func (c *client) status(method, path string, body any) int {
	c.t.Helper()
	status, _, _ := c.do(method, path, body)
	return status
}
