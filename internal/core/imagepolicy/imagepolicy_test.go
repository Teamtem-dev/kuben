package imagepolicy_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/core/imagepolicy"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

func parse(t *testing.T, text string) imagepolicy.Pattern {
	t.Helper()
	p, err := imagepolicy.Parse(text)
	if err != nil {
		t.Fatalf("Parse(%q): %v", text, err)
	}
	return p
}

func TestPatternsParseAndPrint(t *testing.T) {
	if diff := cmp.Diff(imagepolicy.Pattern(imagepolicy.Tag{Tag: "latest"}), parse(t, "latest")); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(imagepolicy.Pattern(imagepolicy.Glob{Glob: "release-*"}), parse(t, " tag:release-* ")); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	texts := []struct{ in, want string }{
		{"latest", "latest"},
		{"  v1.2_x  ", "v1.2_x"},
		{"tag:release-*", "tag:release-*"},
		{"tag:*", "tag:*"},
		{"semver:1.2.*", "semver:1.2.*"},
		{"semver:^1", "semver:^1"},
		{"semver:1.2", "semver:^1.2"},
		{"semver:1.2.3", "semver:^1.2.3"},
		{"semver: >=1.0,<2 ", "semver:>=1.0, <2"},
		{"semver:>=1.2.11-rc.0, <1.3.0", "semver:>=1.2.11-rc.0, <1.3.0"},
		{"semver:>= 1.2", "semver:>=1.2"},
		{"semver:*", "semver:*"},
		{"semver:x", "semver:*"},
		{"semver:1.x", "semver:1.*"},
		{"semver:1.2.X", "semver:1.2.*"},
		{"semver:1.*.*", "semver:1.*"},
		{"semver:>=1.2.*", "semver:>=1.2"},
		{"semver:=1.2.3-rc.1+build.7", "semver:=1.2.3-rc.1"},
		{"semver:~0.0.1", "semver:~0.0.1"},
	}
	for _, c := range texts {
		if got := parse(t, c.in).Text(); got != c.want {
			t.Errorf("Parse(%q).Text() = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"semver:not a range", "bad tag", "-x", ".x", "lat*est", "", "tag:", "tag:a/b", "a:b", strings.Repeat("a", 129)} {
		p, err := imagepolicy.Parse(bad)
		var pe *imagepolicy.PatternError
		if !errors.As(err, &pe) || p != nil {
			t.Errorf("Parse(%q) = %v, %v; globs need the tag: prefix", bad, p, err)
			continue
		}
		if !errors.Is(err, kerr.ErrValidation) {
			t.Errorf("Parse(%q): %v is not a validation error", bad, err)
		}
	}
	if parse(t, "latest").NeedsListing() || !parse(t, "tag:v*").NeedsListing() || !parse(t, "semver:*").NeedsListing() {
		t.Error("only a plain tag needs no listing")
	}
	if got := parse(t, strings.Repeat("a", 128)).Text(); len(got) != 128 {
		t.Errorf("got %d characters", len(got))
	}
}

func TestPatternErrorsReadAsInRust(t *testing.T) {
	cases := []struct{ in, want string }{
		{"bad tag", "`bad tag` is not a valid tag or tag glob"},
		{"tag:a b", "`a b` is not a valid tag or tag glob"},
		{
			"semver:not a range",
			"`not a range` is not a SemVer range: unexpected character 'n' while parsing major version number",
		},
		{"semver: ", "`` is not a SemVer range: unexpected end of input while parsing major version number"},
		// The range is shown as written after the prefix, not trimmed.
		{"semver: 1.2 x", "` 1.2 x` is not a SemVer range: expected comma after minor version number, found 'x'"},
		{"semver:1.2.3\t4", "`1.2.3\t4` is not a SemVer range: expected comma after patch version number, found '\\t'"},
		{"semver:v1.2.3", "`v1.2.3` is not a SemVer range: unexpected character 'v' while parsing major version number"},
		{"semver:1.2.3 4", "`1.2.3 4` is not a SemVer range: expected comma after patch version number, found '4'"},
		{"semver:1.2 || 2", "`1.2 || 2` is not a SemVer range: expected comma after minor version number, found '|'"},
		{"semver:*, 1", "`*, 1` is not a SemVer range: wildcard req (*) must be the only comparator in the version req"},
		{"semver:1, x", "`1, x` is not a SemVer range: wildcard req (x) must be the only comparator in the version req"},
		{"semver:*1", "`*1` is not a SemVer range: unexpected character after wildcard in version req"},
		{"semver:1.*.3", "`1.*.3` is not a SemVer range: unexpected character after wildcard in version req"},
		{"semver:01", "`01` is not a SemVer range: invalid leading zero in major version number"},
		{"semver:1.2.3-", "`1.2.3-` is not a SemVer range: empty identifier segment in pre-release identifier"},
		{"semver:1.2.3-a..b", "`1.2.3-a..b` is not a SemVer range: empty identifier segment in pre-release identifier"},
		{"semver:1.2.3-01", "`1.2.3-01` is not a SemVer range: invalid leading zero in pre-release identifier"},
		{"semver:1.2.3+", "`1.2.3+` is not a SemVer range: empty identifier segment in build metadata"},
		{"semver:1.", "`1.` is not a SemVer range: unexpected end of input while parsing minor version number"},
		{
			"semver:99999999999999999999",
			"`99999999999999999999` is not a SemVer range: value of major version number exceeds u64::MAX",
		},
		{"semver:^1,", "`^1,` is not a SemVer range: unexpected end of input while parsing major version number"},
	}
	for _, c := range cases {
		_, err := imagepolicy.Parse(c.in)
		if err == nil || err.Error() != c.want {
			t.Errorf("Parse(%q):\n got %v\nwant %s", c.in, err, c.want)
		}
	}
	many := strings.TrimSuffix(strings.Repeat(">=1, ", 32), ", ")
	if _, err := imagepolicy.ParseVersionReq(many); err != nil {
		t.Errorf("32 comparators are allowed: %v", err)
	}
	if _, err := imagepolicy.ParseVersionReq(many + ", >=1"); err == nil ||
		err.Error() != "excessive number of version comparators" {
		t.Errorf("got %v", err)
	}
}

// The semantics of Rust's semver crate (Cargo's dialect), where they differ
// from Masterminds/semver or are easy to get wrong.
func TestRangesMatchAsCargoDoes(t *testing.T) {
	cases := []struct {
		req     string
		matches []string
		refuses []string
	}{
		{"1.2.3", []string{"1.2.3", "1.2.4", "1.9.0"}, []string{"1.2.2", "2.0.0", "1.2.4-rc.1"}},
		{"^1.2", []string{"1.2.0", "1.3.5"}, []string{"1.1.9", "2.0.0"}},
		{"^1", []string{"1.0.0", "1.99.0"}, []string{"0.9.0", "2.0.0"}},
		{"^0.2.3", []string{"0.2.3", "0.2.9"}, []string{"0.2.2", "0.3.0", "1.0.0"}},
		{"^0.0.3", []string{"0.0.3"}, []string{"0.0.4", "0.0.2", "0.1.0"}},
		{"^0.0", []string{"0.0.0", "0.0.9"}, []string{"0.1.0"}},
		{"^0.2", []string{"0.2.0", "0.2.9"}, []string{"0.3.0", "0.1.9"}},
		{"^0", []string{"0.0.1", "0.9.9"}, []string{"1.0.0"}},
		{"~1.2.3", []string{"1.2.3", "1.2.9"}, []string{"1.2.2", "1.3.0"}},
		{"~1.2", []string{"1.2.0", "1.2.9"}, []string{"1.3.0", "1.1.0"}},
		{"~1", []string{"1.0.0", "1.9.9"}, []string{"2.0.0", "0.9.0"}},
		{"=1.2.3", []string{"1.2.3", "1.2.3+build"}, []string{"1.2.4", "1.2.3-rc.1"}},
		{"=1.2", []string{"1.2.0", "1.2.9"}, []string{"1.3.0"}},
		{"=1", []string{"1.0.0", "1.9.9"}, []string{"2.0.0"}},
		{"1.2.*", []string{"1.2.0", "1.2.10"}, []string{"1.3.0", "1.2.11-rc.1"}},
		{"1.*", []string{"1.0.0", "1.9.0"}, []string{"2.0.0"}},
		{"*", []string{"0.0.0", "9.9.9"}, []string{"1.0.0-rc.1"}},
		{">1.2.3", []string{"1.2.4", "2.0.0"}, []string{"1.2.3", "1.2.2"}},
		{">1.2", []string{"1.3.0", "2.0.0"}, []string{"1.2.0", "1.2.9", "1.1.0"}},
		{">1", []string{"2.0.0"}, []string{"1.9.9", "1.0.0"}},
		{">=1.2", []string{"1.2.0", "1.2.9", "2.0.0"}, []string{"1.1.9"}},
		{"<1.2", []string{"1.1.9", "0.1.0"}, []string{"1.2.0", "1.2.9"}},
		{"<1.2.3", []string{"1.2.2"}, []string{"1.2.3", "1.2.3-rc.1"}},
		{"<=1.2", []string{"1.2.9", "1.1.0"}, []string{"1.3.0"}},
		{"<=1", []string{"1.9.9", "0.1.0"}, []string{"2.0.0"}},
		{">=1.0, <2", []string{"1.0.0", "1.9.9"}, []string{"0.9.9", "2.0.0"}},
		{
			">=1.2.11-rc.0, <1.3.0",
			[]string{"1.2.11-rc.0", "1.2.11-rc.1", "1.2.11", "1.2.12"},
			[]string{"1.2.12-rc.1", "1.3.0-rc.1", "1.2.10", "1.3.0"},
		},
		{"^1.2.3-beta.2", []string{"1.2.3-beta.2", "1.2.3-beta.10", "1.2.3-rc", "1.2.3", "1.4.0"}, []string{"1.2.3-beta.1", "1.2.3-alpha", "1.2.4-beta.3"}},
		{"~1.2.3-beta.2", []string{"1.2.3-beta.3", "1.2.3", "1.2.4"}, []string{"1.2.3-beta.1", "1.3.0"}},
		{">1.2.3-rc.1", []string{"1.2.3-rc.2", "1.2.3-rc.1.0", "1.2.3"}, []string{"1.2.3-rc.1", "1.2.3-7"}},
		{"<1.2.3-rc.1", []string{"1.2.3-alpha", "1.2.3-99", "1.2.2"}, []string{"1.2.3-rc.1", "1.2.3", "1.2.2-rc.1"}},
	}
	for _, c := range cases {
		req, err := imagepolicy.ParseVersionReq(c.req)
		if err != nil {
			t.Errorf("%q: %v", c.req, err)
			continue
		}
		for _, v := range c.matches {
			if !req.Matches(semver.MustParse(v)) {
				t.Errorf("%q must match %s", c.req, v)
			}
		}
		for _, v := range c.refuses {
			if req.Matches(semver.MustParse(v)) {
				t.Errorf("%q must not match %s", c.req, v)
			}
		}
	}
	if (imagepolicy.VersionReq{}).Matches(nil) {
		t.Error("no version matches nothing")
	}
}

func TestSemverRangesPickTheHighestRelease(t *testing.T) {
	all := []string{"1.2.0", "v1.2.9", "1.2.10", "1.3.0", "1.2.11-rc.1", "latest", "1.2"}
	cases := []struct {
		pattern string
		tags    []string
		want    opt.Val[string]
	}{
		{"semver:1.2.*", all, opt.Some("1.2.10")},
		{"semver:^1", all, opt.Some("1.3.0")},
		{"semver:>=1.2.11-rc.0, <1.3.0", all, opt.Some("1.2.11-rc.1")}, // Prereleases when asked for.
		{"semver:^2", all, opt.None[string]()},
		{"semver:~1.2", []string{"1.2"}, opt.Some("1.2")}, // Short versions count.
		{"semver:^1", []string{"1"}, opt.Some("1")},
		{"semver:~1.2", []string{"v1.2.9", "1.2.10"}, opt.Some("1.2.10")},
		{"semver:~1.2", []string{"1.2.10", "v1.2.9"}, opt.Some("1.2.10")},
		// Among equal versions the last one listed wins, as Rust's max_by.
		{"semver:~1.2", []string{"1.2.0", "v1.2.0", "1.2"}, opt.Some("1.2")},
		{"semver:~1.2", []string{"1.2", "v1.2.0", "1.2.0"}, opt.Some("1.2.0")},
		// Build metadata breaks ties the way Rust's Version order does.
		{"semver:*", []string{"1.0.0+2", "1.0.0+10", "1.0.0+a", "1.0.0"}, opt.Some("1.0.0+a")},
		{"semver:*", []string{"1.0.0+01", "1.0.0+1"}, opt.Some("1.0.0+01")},
		// Strict SemVer only: none of these is a version.
		{"semver:*", []string{"vv1.2.3", "V1.2.3", "01.2.3", "1.02", "1.2.3.4", "1.2-rc", "1.2.3-", "1.2.3-01", "", "v", "1.x", "latest"}, opt.None[string]()},
		{"semver:*", []string{"1.0.0-rc.1", "1.0.0-rc.2"}, opt.None[string]()},
		{"semver:>=1.0.0-rc.1", []string{"1.0.0-rc.1", "1.0.0-rc.10", "1.0.0-rc.9", "1.0.0-beta"}, opt.Some("1.0.0-rc.10")},
		{"semver:*", nil, opt.None[string]()},
	}
	for _, c := range cases {
		if got := parse(t, c.pattern).Select(c.tags); got != c.want {
			t.Errorf("%s among %v: got %v, want %v", c.pattern, c.tags, got, c.want)
		}
	}
}

func TestGlobsPickTheHighestMatch(t *testing.T) {
	all := []string{"release-9", "release-10", "release-2", "nightly-99"}
	if got := parse(t, "tag:release-*").Select(all); got != opt.Some("release-10") {
		t.Errorf("got %v", got)
	}
	if got := parse(t, "tag:stable-*").Select(all); got.IsSome() {
		t.Errorf("got %v", got)
	}
	if got := parse(t, "tag:r-*").Select([]string{"r-10", "r-010"}); got != opt.Some("r-010") {
		t.Errorf("among equals the last one wins: got %v", got)
	}
	if got := (imagepolicy.Tag{Tag: "latest"}).Select(nil); got != opt.Some("latest") {
		t.Errorf("got %v", got)
	}
	globs := []struct {
		glob, tag string
		want      bool
	}{
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXcYYb", false},
		{"ab*ba", "aba", false},
		{"ab*ba", "abba", true},
		{"ab*ba", "abXba", true},
		{"*", "", true},
		{"*", "anything/at:all", true},
		{"**", "x", true},
		{"a**b", "ab", true},
		{"release-*", "release-", true},
		{"release-*", "release", false},
		{"*-rc", "1.2-rc", true},
		{"*-rc", "1.2-rc1", false},
		{"v1", "v1", true},
		{"v1", "v10", false},
		{"v?", "v1", false},     // Only `*` is special.
		{"v[0-9]", "v1", false}, // No character classes.
		{"a*a*a", "aa", false},
		{"a*a*a", "aaa", true},
		{"*b*", "abc", true},
		{"*é*", "café-1", true},
	}
	for _, c := range globs {
		if got := imagepolicy.GlobMatches(c.glob, c.tag); got != c.want {
			t.Errorf("GlobMatches(%q, %q) = %v", c.glob, c.tag, got)
		}
	}
}

func TestNaturalOrderComparesDigitRunsAsNumbers(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"r-9", "r-10", -1},
		{"r-10", "r-9", 1},
		{"r-10", "r-010", 0},
		{"r-1", "r-1a", -1},
		{"a", "b", -1},
		{"", "", 0},
		{"", "a", -1},
		{"1.2.10", "1.2.9", 1},
		{"1a", "a1", -1},
		{"99999999999999999999999", "100000000000000000000000", -1},
		{"r-0", "r-00", 0},
	}
	for _, c := range cases {
		if got := imagepolicy.Natural(c.a, c.b); got != c.want {
			t.Errorf("Natural(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestChecksBackOffAfterFailures(t *testing.T) {
	none := opt.None[uint64]()
	cases := []struct {
		name               string
		now                int64
		interval, failures uint32
		retryAfter         opt.Val[uint64]
		want               int64
	}{
		{"the interval", 0, 300, 0, none, 300_000},
		{"doubling", 0, 300, 2, none, 1_200_000},
		{"a day at most", 0, 300, 30, none, 86_400_000},
		{"a day at most, whatever the count", 0, 300, math.MaxUint32, none, 86_400_000},
		{"a minute at least", 0, 10, 0, none, 60_000},
		{"a day at most between checks", 0, math.MaxUint32, 0, none, 86_400_000},
		{"the registry's wish", 0, 300, 0, opt.Some[uint64](900), 900_000},
		{"a shorter wish changes nothing", 0, 300, 0, opt.Some[uint64](10), 300_000},
		{"a wish beyond the ceiling is kept", 5, 300, 0, opt.Some[uint64](100_000), 100_000_005},
		{"an absurd wish saturates", 1, 300, 0, opt.Some[uint64](math.MaxUint64), math.MaxInt64},
		{"the end of time saturates", math.MaxInt64 - 1, 300, 0, none, math.MaxInt64},
	}
	for _, c := range cases {
		if got := imagepolicy.NextCheck(c.now, c.interval, c.failures, c.retryAfter); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	if imagepolicy.MinIntervalSecs != 60 || imagepolicy.MaxIntervalSecs != 86_400 {
		t.Error("the bounds are part of the contract")
	}
}

// A printed range is its own normal form, and printing never loses a match.
func FuzzRangesPrintTheirNormalForm(f *testing.F) {
	for _, seed := range []string{"^1.2", "1.2.*", ">=1.0, <2", "*", " >= 1.2.3-rc.1+b ,<2", "1.x.X", "~0", "=1.2.3-0.a"} {
		f.Add(seed)
	}
	probes := []*semver.Version{
		semver.MustParse("0.0.0"), semver.MustParse("1.2.3"), semver.MustParse("1.2.3-rc.1"),
		semver.MustParse("1.9.0"), semver.MustParse("2.0.0"),
	}
	f.Fuzz(func(t *testing.T, text string) {
		req, err := imagepolicy.ParseVersionReq(text)
		if err != nil {
			return
		}
		again, err := imagepolicy.ParseVersionReq(req.String())
		if err != nil || again.String() != req.String() {
			t.Fatalf("%q prints %q, which reads as %q, %v", text, req, again, err)
		}
		for _, v := range probes {
			if req.Matches(v) != again.Matches(v) {
				t.Fatalf("%q and %q disagree on %s", text, req, v)
			}
		}
	})
}

// A glob made from a tag by cutting a piece out matches that tag.
func FuzzGlobsMatchWhatTheyWereCutFrom(f *testing.F) {
	f.Add("release-10", uint8(3), uint8(5))
	f.Add("", uint8(0), uint8(0))
	f.Add("aXXbYYc", uint8(1), uint8(6))
	f.Fuzz(func(t *testing.T, tag string, from, to uint8) {
		if strings.Contains(tag, "*") {
			return
		}
		if !imagepolicy.GlobMatches(tag, tag) || !imagepolicy.GlobMatches("*", tag) {
			t.Fatalf("%q must match itself and `*`", tag)
		}
		i, j := min(int(from), len(tag)), min(int(to), len(tag))
		if i > j {
			i, j = j, i
		}
		glob := tag[:i] + "*" + tag[j:]
		if !imagepolicy.GlobMatches(glob, tag) {
			t.Fatalf("%q must match %q", glob, tag)
		}
		if imagepolicy.GlobMatches(tag, tag+"x") {
			t.Fatalf("%q without a star matches only itself", tag)
		}
	})
}
