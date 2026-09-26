package source_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

var cmpValues = cmp.AllowUnexported(
	source.CommitSha{}, source.RepoName{}, source.BranchName{}, source.RepoPath{}, opt.Val[source.RepoPath]{},
)

func invalidKind(err error) source.InvalidKind {
	var inv *source.Invalid
	if !errors.As(err, &inv) || !errors.Is(err, kerrors.ErrValidation) {
		return ""
	}
	return inv.Kind
}

func repoPath(t *testing.T, s string) source.RepoPath {
	t.Helper()
	p, err := source.ParseRepoPath(s)
	if err != nil {
		t.Fatalf("ParseRepoPath(%q): %v", s, err)
	}
	return p
}

func TestCommitsAreFullLowercaseShas(t *testing.T) {
	c, err := source.ParseCommitSha(sha)
	if err != nil || c.String() != sha || !c.Valid() || c.IsZero() {
		t.Fatalf("got %v, %v", c, err)
	}
	if got := c.Short(); got != "0123456789ab" {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{"", "0123456", strings.ToUpper(sha), sha + "0", sha[:39] + "g", sha[:38] + "é"} {
		if _, err := source.ParseCommitSha(bad); invalidKind(err) != source.InvalidCommit {
			t.Errorf("%q: got %v", bad, err)
		}
	}
	zero, err := source.ParseCommitSha(strings.Repeat("0", 40))
	if err != nil || !zero.IsZero() {
		t.Errorf("got %v, %v", zero, err)
	}
	var none source.CommitSha
	if none.Valid() || none.IsZero() || none.Short() != "" {
		t.Error("the zero value is no commit")
	}
}

func TestRepositoriesAreOwnerSlashName(t *testing.T) {
	r, err := source.ParseRepoName("Acme/Shop.Web")
	if err != nil || r.String() != "acme/shop.web" || r.Owner() != "acme" || r.Name() != "shop.web" || !r.Valid() {
		t.Fatalf("got %v, %v", r, err)
	}
	bad := []string{
		"", "acme", "/shop", "acme/", "acme/shop/x", "acme/..", "./shop", "ac me/shop", "acmé/shop",
		strings.Repeat("a", 101) + "/shop",
	}
	for _, s := range bad {
		if _, err := source.ParseRepoName(s); invalidKind(err) != source.InvalidRepository {
			t.Errorf("%q: got %v", s, err)
		}
	}
	if _, err := source.ParseRepoName(strings.Repeat("a", 100) + "/a-b_c.d"); err != nil {
		t.Error(err)
	}
	var none source.RepoName
	if none.Valid() || none.Owner() != "" || none.Name() != "" {
		t.Error("the zero value is no repository")
	}
}

func TestBranchesFollowRefFormat(t *testing.T) {
	for _, good := range []string{"main", "release/1.2", "feat/x-y_z", "weiß/ünï", "a@b", "a{b", strings.Repeat("a", 255)} {
		if b, err := source.ParseBranchName(good); err != nil || b.String() != good || !b.Valid() {
			t.Errorf("%q: got %v, %v", good, b, err)
		}
	}
	bad := []string{
		"", "-x", ".x", "a..b", "a b", "a\tb", "a b", "a\u0085b", "a\u007fb", "a~1", "a^", "a?", "a*", "a[", "a\\b",
		"x.lock", "a/", "a.", "/a", "a//b", "a/.hidden", "a@{1}", "a:b", "a\xffb", strings.Repeat("a", 256),
	}
	for _, s := range bad {
		if _, err := source.ParseBranchName(s); invalidKind(err) != source.InvalidBranch {
			t.Errorf("%q: got %v", s, err)
		}
	}
	if b, ok := source.BranchFromRef("refs/heads/main"); !ok || b.String() != "main" {
		t.Errorf("got %v, %v", b, ok)
	}
	for _, ref := range []string{"refs/tags/v1", "main", "refs/heads/", "refs/heads/a..b"} {
		if b, ok := source.BranchFromRef(ref); ok || b.Valid() {
			t.Errorf("%q: got %v", ref, b)
		}
	}
}

func TestRepoPathsNeverEscapeTheCheckout(t *testing.T) {
	for _, root := range []string{"", "./", ".", "/", "././", ".//"} {
		if !repoPath(t, root).IsRoot() {
			t.Errorf("%q is the root", root)
		}
	}
	paths := []struct{ in, want string }{
		{"./apps/web/", "apps/web"},
		{"././apps", "apps"},
		{"apps/.hidden/x y", "apps/.hidden/x y"},
		{strings.Repeat("a", 512), strings.Repeat("a", 512)},
	}
	for _, c := range paths {
		if got := repoPath(t, c.in).String(); got != c.want {
			t.Errorf("%q: got %q", c.in, got)
		}
	}
	bad := []string{"../x", "a/../../b", "/etc", "a//b", "a\\b", "a/./b", "a/\nb", ".//a", "a\xff", strings.Repeat("a", 513)}
	for _, s := range bad {
		if _, err := source.ParseRepoPath(s); invalidKind(err) != source.InvalidPath {
			t.Errorf("%q: got %v", s, err)
		}
	}
	ctx := repoPath(t, "apps/web")
	recipe := source.BuildRecipe{Context: ctx}
	if got := ctx.Join(recipe.DockerfilePath()).String(); got != "apps/web/Dockerfile" {
		t.Errorf("got %q", got)
	}
	recipe.Dockerfile = opt.Some(repoPath(t, "build/Dockerfile"))
	if got := ctx.Join(recipe.DockerfilePath()).String(); got != "apps/web/build/Dockerfile" {
		t.Errorf("got %q", got)
	}
	if (source.RepoPath{}).Join(ctx) != ctx || ctx.Join(source.RepoPath{}) != ctx {
		t.Error("joining the root changes nothing")
	}
}

func TestMessagesArePinned(t *testing.T) {
	cases := []struct {
		err  source.Invalid
		want string
	}{
		{source.Invalid{Kind: source.InvalidCommit, Value: "abc"}, `not a full 40-character commit SHA: "abc"`},
		{source.Invalid{Kind: source.InvalidRepository, Value: "ac me"}, "not an `owner/name` repository: \"ac me\""},
		{
			source.Invalid{Kind: source.InvalidBranch, Value: "a\"b\\c\n\u0001é"},
			`not a valid branch name: "a\"b\\c\n\u{1}é"`,
		},
		{source.Invalid{Kind: source.InvalidPath, Value: "../x"}, `not a relative path inside the repository: "../x"`},
		{
			source.Invalid{Kind: source.InvalidPayload, Value: "a push without an installation"},
			"the webhook payload is malformed: a push without an installation",
		},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
}

func TestBuildStrategiesParse(t *testing.T) {
	for _, s := range []source.BuildStrategy{source.Auto, source.Dockerfile, source.Railpack} {
		if got, err := source.ParseBuildStrategy(string(s)); err != nil || got != s {
			t.Errorf("%q: got %q, %v", s, got, err)
		}
	}
	_, err := source.ParseBuildStrategy("Nixpacks")
	if invalidKind(err) != source.InvalidPayload ||
		err.Error() != `the webhook payload is malformed: unknown build strategy "Nixpacks"` {
		t.Errorf("got %v", err)
	}
	if source.BuildStrategy("").OrAuto() != source.Auto || source.Railpack.OrAuto() != source.Railpack {
		t.Error("only the zero value reads as auto")
	}
}

func TestRecipesRoundTripAsJSON(t *testing.T) {
	var recipe source.BuildRecipe
	in := `{"strategy": "dockerfile", "context": "./svc/", "dockerfile": "build/Dockerfile", "other": 1}`
	if err := json.Unmarshal([]byte(in), &recipe); err != nil {
		t.Fatal(err)
	}
	want := source.BuildRecipe{
		Strategy:   source.Dockerfile,
		Context:    repoPath(t, "svc"),
		Dockerfile: opt.Some(repoPath(t, "build/Dockerfile")),
	}
	if diff := cmp.Diff(want, recipe, cmpValues); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	wire, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(wire); got != `{"strategy":"dockerfile","context":"svc","dockerfile":"build/Dockerfile"}` {
		t.Errorf("got %s", got)
	}
	var back source.BuildRecipe
	if err := json.Unmarshal(wire, &back); err != nil || back != recipe {
		t.Errorf("got %v, %v", back, err)
	}
}

func TestRecipeDefaultsAndRefusals(t *testing.T) {
	// The zero recipe and the empty object are the same plan, written in full.
	wire, err := json.Marshal(source.BuildRecipe{})
	if err != nil || string(wire) != `{"strategy":"auto","context":""}` {
		t.Errorf("got %s, %v", wire, err)
	}
	for _, in := range []string{`{}`, `{"dockerfile": null}`, `{"strategy":"auto","context":""}`} {
		var recipe source.BuildRecipe
		if err := json.Unmarshal([]byte(in), &recipe); err != nil || recipe != (source.BuildRecipe{Strategy: source.Auto}) {
			t.Errorf("%s: got %+v, %v", in, recipe, err)
		}
	}
	refused := []string{
		`{"context": "../x"}`, `{"dockerfile": "/etc/passwd"}`, `{"strategy": "nixpacks"}`, `{"strategy": "Auto"}`,
		`{"strategy": null}`, `{"context": null}`, `{"context": 1}`, `[]`,
	}
	for _, in := range refused {
		var recipe source.BuildRecipe
		if err := json.Unmarshal([]byte(in), &recipe); err == nil {
			t.Errorf("%s: got %+v", in, recipe)
		}
	}
}

func TestValuesAreJSONStrings(t *testing.T) {
	type all struct {
		Commit source.CommitSha  `json:"commit"`
		Repo   source.RepoName   `json:"repo"`
		Branch source.BranchName `json:"branch"`
		Path   source.RepoPath   `json:"path"`
	}
	var v all
	in := `{"commit":"` + sha + `","repo":"Acme/Shop","branch":"feat/x","path":"./apps/web/"}`
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(v)
	want := `{"commit":"` + sha + `","repo":"acme/shop","branch":"feat/x","path":"apps/web"}`
	if err != nil || string(wire) != want {
		t.Errorf("got %s, %v", wire, err)
	}
	refused := []struct {
		in   string
		kind source.InvalidKind
	}{
		{`{"commit":"abc"}`, source.InvalidCommit},
		{`{"repo":"acme"}`, source.InvalidRepository},
		{`{"branch":"a..b"}`, source.InvalidBranch},
		{`{"path":"../x"}`, source.InvalidPath},
		{`{"commit":null}`, ""},
		{`{"repo":7}`, ""},
		{`{"branch":["main"]}`, ""},
		{`{"path":{"a":"b"}}`, ""},
	}
	for _, c := range refused {
		var v all
		err := json.Unmarshal([]byte(c.in), &v)
		if err == nil || invalidKind(err) != c.kind {
			t.Errorf("%s: got %v", c.in, err)
		}
	}
}
