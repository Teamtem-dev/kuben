package oracle_test

import (
	"os"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/test/oracle"
)

// The walking skeleton (G2): first-run setup, sign-in, the session, the
// project list, sign-out, and the error paths around them. More scenarios
// join with every slice.
var skeleton = []oracle.Step{
	{Name: "setup status", Method: "GET", Path: "/api/v1/setup"},
	{Name: "me before setup", Method: "GET", Path: "/api/v1/me"},
	{Name: "setup without a name", Method: "POST", Path: "/api/v1/setup", Body: map[string]any{
		"org_name": " ", "email": "admin@example.com", "password": "correct horse battery staple",
	}},
	{Name: "setup", Method: "POST", Path: "/api/v1/setup", Body: map[string]any{
		"org_name": "ACME", "email": "Admin@Example.com", "password": "correct horse battery staple",
	}},
	{Name: "setup again", Method: "POST", Path: "/api/v1/setup", Body: map[string]any{
		"org_name": "ACME", "email": "b@example.com", "password": "correct horse battery staple",
	}},
	{Name: "me after setup", Method: "GET", Path: "/api/v1/me"},
	{Name: "projects", Method: "GET", Path: "/api/v1/projects"},
	{Name: "unknown project", Method: "GET", Path: "/api/v1/projects/nope"},
	{Name: "unknown API route", Method: "GET", Path: "/api/v1/nothing-here"},
	{Name: "logout", Method: "POST", Path: "/api/v1/auth/logout"},
	{Name: "me after logout", Method: "GET", Path: "/api/v1/me"},
	{Name: "wrong password", Method: "POST", Path: "/api/v1/auth/login", Body: map[string]any{
		"email": "admin@example.com", "password": "wrong",
	}},
	{Name: "login", Method: "POST", Path: "/api/v1/auth/login", Body: map[string]any{
		"email": "ADMIN@example.com ", "password": "correct horse battery staple",
	}},
	{Name: "me after login", Method: "GET", Path: "/api/v1/me"},
	{
		Name: "mutation without the client header", Method: "POST", Path: "/api/v1/auth/logout",
		Headers: map[string]string{"X-Kuben-Client": ""},
	},
	{Name: "console page", Method: "GET", Path: "/projects/shop"},
	{Name: "livez", Method: "GET", Path: "/livez"},
}

// TestSkeletonMatchesRust needs both servers running on empty databases:
// KUBEN_ORACLE_RUST and KUBEN_ORACLE_GO are their base URLs.
func TestSkeletonMatchesRust(t *testing.T) {
	rustURL, goURL := os.Getenv("KUBEN_ORACLE_RUST"), os.Getenv("KUBEN_ORACLE_GO")
	if rustURL == "" || goURL == "" {
		t.Skip("set KUBEN_ORACLE_RUST and KUBEN_ORACLE_GO to compare the servers")
	}
	rust, err := oracle.NewClient(rustURL)
	if err != nil {
		t.Fatal(err)
	}
	goServer, err := oracle.NewClient(goURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range skeleton {
		want, err := rust.Do(step)
		if err != nil {
			t.Fatal(err)
		}
		got, err := goServer.Do(step)
		if err != nil {
			t.Fatal(err)
		}
		if d := oracle.Diff(want, got); d != "" {
			t.Errorf("%s: %s", step.Name, d)
		}
	}
}

func TestNormalize(t *testing.T) {
	in := `id 0192f3a1-0000-7000-8000-000000000001 at 2026-09-21T10:00:00.123Z token kbn_pat_0192f3a100007000800000000000000a_abc-DEF_1`
	want := `id <uuid> at <time> token <token>`
	if got := oracle.Normalize(in); got != want {
		t.Fatalf("got %s", got)
	}
}
