package upgrade_test

import (
	"errors"
	"math"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/upgrade"
)

var cmpOpt = cmp.AllowUnexported(opt.Val[string]{}, opt.Val[int64]{}, opt.Val[uint32]{}, opt.Val[uint64]{})

func version(t *testing.T, s string) upgrade.Version {
	t.Helper()
	v, ok := upgrade.ParseVersion(s)
	if !ok {
		t.Fatalf("%q does not parse", s)
	}
	return v
}

func TestVersionsParseAndOrder(t *testing.T) {
	want := upgrade.Version{Major: 1, Minor: 2, Patch: 3}
	for _, s := range []string{"v1.2.3", "1.2.3", " vv1.2.3 ", "1.2.3+build.5", "01.2.3"} {
		if diff := cmp.Diff(want, version(t, s), cmpOpt); diff != "" {
			t.Errorf("%q (-want +got):\n%s", s, diff)
		}
	}
	if pre := version(t, "1.2.3-rc.1+build.5").Pre; pre != opt.Some("rc.1") {
		t.Errorf("got %v", pre)
	}
	if pre := version(t, "1.2.3-rc-1").Pre; pre != opt.Some("rc-1") {
		t.Errorf("got %v", pre)
	}
	order := []struct {
		a, b string
		want int
	}{
		{"1.2.3-rc.1", "1.2.3", -1},
		{"1.2.3", "1.2.3-rc.1", 1},
		{"1.10.0", "1.9.9", 1},
		{"1.2.3-alpha", "1.2.3-beta", -1},
		{"1.2.3-rc.1", "1.2.3-rc.1", 0},
		{"2.0.0", "1.99.99", 1},
		{"1.2.3", "v1.2.3+x", 0},
	}
	for _, c := range order {
		if got := version(t, c.a).Compare(version(t, c.b)); got != c.want {
			t.Errorf("%s vs %s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if got := version(t, "2.0.0-beta").String(); got != "2.0.0-beta" {
		t.Errorf("got %q", got)
	}
	if got := version(t, "v2.1.0+build").String(); got != "2.1.0" {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "a.b.c", "1.2.3-", "1..3", "+1.2.3", "1.2.-3", "1.2.3 4"} {
		if v, ok := upgrade.ParseVersion(bad); ok {
			t.Errorf("%q parsed as %v", bad, v)
		}
	}
}

func TestUpgradesGoForwardOneMinorAtATime(t *testing.T) {
	cases := []struct {
		from, to string
		major    bool
		want     upgrade.Step
		refused  upgrade.PathErrorKind
	}{
		{"1.2.3", "1.2.3", false, upgrade.Same, ""},
		{"1.2.3", "1.2.9", false, upgrade.Patch, ""},
		{"1.2.3", "1.3.0", false, upgrade.Minor, ""},
		{"1.3.0-rc.1", "1.3.0", false, upgrade.Patch, ""},
		{"1.3.0", "1.2.9", false, "", upgrade.Downgrade},
		{"1.3.0", "1.3.0-rc.1", false, "", upgrade.Downgrade},
		{"2.0.0", "1.9.0", true, "", upgrade.Downgrade},
		{"1.2.3", "1.4.0", false, "", upgrade.SkipsMinor},
		{"1.9.0", "2.0.0", false, "", upgrade.MajorNeedsConsent},
		{"1.9.0", "2.0.0", true, upgrade.Major, ""},
		{"1.9.0", "3.0.0", true, "", upgrade.SkipsMajor},
		{"1.9.0", "3.0.0", false, "", upgrade.SkipsMajor},
		{"x", "1.0.0", false, "", upgrade.InvalidVersion},
		{"1.0.0", "y", false, "", upgrade.InvalidVersion},
	}
	for _, c := range cases {
		got, err := upgrade.StepBetween(c.from, c.to, c.major)
		if c.refused == "" {
			if err != nil || got != c.want {
				t.Errorf("%s → %s = %q, %v; want %q", c.from, c.to, got, err, c.want)
			}
			continue
		}
		var pe *upgrade.PathError
		if !errors.As(err, &pe) || pe.Kind != c.refused || got != "" {
			t.Errorf("%s → %s = %q, %v; want %s", c.from, c.to, got, err, c.refused)
		}
		if !errors.Is(err, kerrors.ErrValidation) {
			t.Errorf("%s → %s: %v is not a validation error", c.from, c.to, err)
		}
	}
}

func TestRefusalsNameTheWayForward(t *testing.T) {
	cases := []struct {
		from, to string
		want     string
	}{
		{"x", "1.0.0", "`x` is not a version"},
		{"v1.3.0", "1.2.9", "1.3.0 → 1.2.9 goes back: a newer Kuben's database is not run by an older one"},
		{"1.2.3", "1.4.0-rc.1", "1.2.3 → 1.4.0-rc.1 skips minor versions: upgrade to 1.3 first"},
		{"1.9.0", "2.0.0", "1.9.0 → 2.0.0 is a major upgrade: read its release notes, then pass --major"},
		{"1.9.0", "3.0.0", "1.9.0 → 3.0.0 skips a major version"},
	}
	for _, c := range cases {
		_, err := upgrade.StepBetween(c.from, c.to, false)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s → %s: got %v, want %q", c.from, c.to, err, c.want)
		}
	}
	_, err := upgrade.StepBetween("1.2.3", "1.4.0", false)
	var pe *upgrade.PathError
	if !errors.As(err, &pe) || pe.FromMajor != 1 || pe.NextMinor != 3 {
		t.Errorf("got %+v", pe)
	}
}

func TestStepsAndLevelsParse(t *testing.T) {
	for _, s := range []upgrade.Step{upgrade.Same, upgrade.Patch, upgrade.Minor, upgrade.Major} {
		if got, err := upgrade.ParseStep(string(s)); err != nil || got != s {
			t.Errorf("ParseStep(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := upgrade.ParseStep("minor"); !errors.Is(err, kerrors.ErrValidation) {
		t.Errorf("got %v", err)
	}
	ordered := []upgrade.Level{upgrade.LevelOk, upgrade.LevelWarn, upgrade.LevelFail}
	for i, l := range ordered {
		if got, err := upgrade.ParseLevel(string(l)); err != nil || got != l {
			t.Errorf("ParseLevel(%q) = %q, %v", l, got, err)
		}
		if i > 0 && ordered[i-1].Rank() >= l.Rank() {
			t.Errorf("%s must rank above %s", l, ordered[i-1])
		}
	}
	if _, err := upgrade.ParseLevel("FAIL"); !errors.Is(err, kerrors.ErrValidation) {
		t.Errorf("got %v", err)
	}
	if upgrade.Level("other").Rank() >= upgrade.LevelOk.Rank() {
		t.Error("an unknown level ranks below ok")
	}
}

func facts() upgrade.Facts {
	return upgrade.Facts{
		Running:          opt.Some("1.2.0"),
		Target:           "1.3.0",
		Schema:           24,
		TargetSchema:     26,
		LastBackup:       opt.Some[int64](1_000),
		Now:              1_000 + 3_600_000,
		ActiveOperations: 0,
		Agents:           []upgrade.Agent{{Cluster: "primary", Protocol: opt.Some[uint32](1)}},
		TargetProtocols:  upgrade.ProtocolRange{Min: 1, Max: 2},
		FreeBytes:        opt.Some[uint64](10 << 30),
	}
}

func level(t *testing.T, findings []upgrade.Finding, check string) upgrade.Level {
	t.Helper()
	for _, f := range findings {
		if f.Check == check {
			return f.Level
		}
	}
	t.Fatalf("no %s finding", check)
	return ""
}

func TestAPreparedUpgradePasses(t *testing.T) {
	want := []upgrade.Finding{
		{Level: upgrade.LevelOk, Check: "version", Detail: "1.2.0 → 1.3.0 (Minor)"},
		{Level: upgrade.LevelOk, Check: "schema", Detail: "24 → 26 (2 migration(s))"},
		{Level: upgrade.LevelOk, Check: "backup", Detail: "the newest good backup is 1 h old"},
		{Level: upgrade.LevelOk, Check: "operations", Detail: "none in flight"},
		{Level: upgrade.LevelOk, Check: "agents", Detail: "1 linked, all compatible"},
		{Level: upgrade.LevelOk, Check: "disk", Detail: "10 GiB free"},
	}
	if diff := cmp.Diff(want, upgrade.Preflight(facts())); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestWhatWouldBreakTheUpgradeFailsIt(t *testing.T) {
	cases := []struct {
		name   string
		change func(*upgrade.Facts)
		check  string
		want   upgrade.Level
	}{
		{"skipped minor", func(f *upgrade.Facts) { f.Running = opt.Some("1.4.0") }, "version", upgrade.LevelFail},
		{"schema ahead", func(f *upgrade.Facts) { f.Schema = 30 }, "schema", upgrade.LevelFail},
		{"dirty schema", func(f *upgrade.Facts) { f.DirtySchema = true }, "schema", upgrade.LevelFail},
		{"no backup", func(f *upgrade.Facts) { f.LastBackup = opt.None[int64]() }, "backup", upgrade.LevelFail},
		{"old backup", func(f *upgrade.Facts) { f.Now = 1_000 + upgrade.BackupFreshMs + 1 }, "backup", upgrade.LevelFail},
		{"backup just fresh", func(f *upgrade.Facts) { f.Now = 1_000 + upgrade.BackupFreshMs }, "backup", upgrade.LevelOk},
		{"nothing pending", func(f *upgrade.Facts) {
			f.LastBackup = opt.None[int64]()
			f.Schema = 26
		}, "backup", upgrade.LevelWarn},
		{"stale agent", func(f *upgrade.Facts) {
			f.TargetProtocols = upgrade.ProtocolRange{Min: 2, Max: 3}
		}, "agents", upgrade.LevelFail},
		{"never linked yet", func(f *upgrade.Facts) {
			f.Agents = []upgrade.Agent{{Cluster: "edge"}}
		}, "agents", upgrade.LevelOk},
		{"busy", func(f *upgrade.Facts) { f.ActiveOperations = 3 }, "operations", upgrade.LevelWarn},
		{"little space", func(f *upgrade.Facts) { f.FreeBytes = opt.Some[uint64](1 << 20) }, "disk", upgrade.LevelWarn},
		{"unknown space", func(f *upgrade.Facts) { f.FreeBytes = opt.None[uint64]() }, "disk", upgrade.LevelWarn},
		{"first start", func(f *upgrade.Facts) { f.Running = opt.None[string]() }, "version", upgrade.LevelOk},
		{"same again", func(f *upgrade.Facts) { f.Running = opt.Some("1.3.0") }, "version", upgrade.LevelOk},
	}
	for _, c := range cases {
		f := facts()
		c.change(&f)
		if got := level(t, upgrade.Preflight(f), c.check); got != c.want {
			t.Errorf("%s: %s is %s, want %s", c.name, c.check, got, c.want)
		}
	}
}

func TestFindingsAreSortedAndWorded(t *testing.T) {
	f := facts()
	f.Running = opt.Some("1.4.0")
	f.ActiveOperations = 3
	f.FreeBytes = opt.Some[uint64](5 << 20)
	f.Agents = []upgrade.Agent{
		{Cluster: "primary", Protocol: opt.Some[uint32](1)},
		{Cluster: "edge", Protocol: opt.Some[uint32](0)},
		{Cluster: "new"},
	}
	f.TargetProtocols = upgrade.ProtocolRange{Min: 2, Max: 3}
	want := []upgrade.Finding{
		{
			Level: upgrade.LevelFail, Check: "version",
			Detail: "1.4.0 → 1.3.0 goes back: a newer Kuben's database is not run by an older one",
		},
		{
			Level: upgrade.LevelFail, Check: "agents",
			Detail: "primary (protocol 1), edge (protocol 0) would be refused by 1.3.0 (it speaks 2–3): upgrade them first",
		},
		{
			Level: upgrade.LevelWarn, Check: "operations",
			Detail: "3 in flight: they resume after the upgrade, but a quiet moment is safer",
		},
		{Level: upgrade.LevelWarn, Check: "disk", Detail: "only 5 MiB free for backups and state"},
		{Level: upgrade.LevelOk, Check: "schema", Detail: "24 → 26 (2 migration(s))"},
		{Level: upgrade.LevelOk, Check: "backup", Detail: "the newest good backup is 1 h old"},
	}
	if diff := cmp.Diff(want, upgrade.Preflight(f)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	other := facts()
	other.Running = opt.None[string]()
	other.Schema = 30
	other.LastBackup = opt.None[int64]()
	other.FreeBytes = opt.None[uint64]()
	got := upgrade.Preflight(other)
	wantOther := []upgrade.Finding{
		{
			Level: upgrade.LevelFail, Check: "schema",
			Detail: "the database is at schema 30, newer than 1.3.0 knows (26): run a newer Kuben",
		},
		{Level: upgrade.LevelWarn, Check: "backup", Detail: "no backup from the last 24 h (no migrations pending)"},
		{Level: upgrade.LevelWarn, Check: "disk", Detail: "free space unknown"},
		{Level: upgrade.LevelOk, Check: "version", Detail: "first start of 1.3.0"},
		{Level: upgrade.LevelOk, Check: "operations", Detail: "none in flight"},
		{Level: upgrade.LevelOk, Check: "agents", Detail: "1 linked, all compatible"},
	}
	if diff := cmp.Diff(wantOther, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestExtremeTimesDoNotWrap(t *testing.T) {
	f := facts()
	f.Now = math.MaxInt64
	f.LastBackup = opt.Some[int64](math.MinInt64)
	if got := level(t, upgrade.Preflight(f), "backup"); got != upgrade.LevelFail {
		t.Errorf("got %s", got)
	}
	if !(upgrade.ProtocolRange{Min: 1, Max: 2}).Contains(2) || (upgrade.ProtocolRange{Min: 3, Max: 2}).Contains(2) {
		t.Error("ranges are inclusive, and empty when reversed")
	}
	if upgrade.BackupFreshMs != 86_400_000 || upgrade.MinFreeBytes != 1<<30 {
		t.Error("the thresholds are part of the contract")
	}
}
