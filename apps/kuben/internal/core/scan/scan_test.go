package scan_test

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/scan"
)

const (
	digest       = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	now    int64 = 1_800_000_000_000
)

var optCmp = cmp.AllowUnexported(opt.Val[string]{}, opt.Val[int64]{})

func summary(counts scan.Counts, findings []scan.Finding, ageSecs int64) scan.Summary {
	return scan.Summary{
		Status:      scan.StatusOK,
		Scanner:     "trivy 0.74.0",
		DBUpdatedAt: opt.Some(now - 3_600_000),
		Counts:      counts,
		Findings:    findings,
		ScannedAt:   now - ageSecs*1000,
	}
}

func one(s opt.Val[scan.Summary]) []scan.Scanned {
	return []scan.Scanned{{Digest: digest, Scan: s}}
}

func except(ids ...string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// blocked is the first reason of a Block verdict.
func blocked(t *testing.T, v scan.Verdict) string {
	t.Helper()
	b, ok := v.(scan.Block)
	if !ok || len(b.Reasons) == 0 {
		t.Fatalf("got %#v, want a block with reasons", v)
	}
	return b.Reasons[0]
}

func TestReportsListOnlyWellFormedFindings(t *testing.T) {
	var report scan.Report
	err := json.Unmarshal([]byte(`{"status":"ok","scanner":"trivy 0.74.0","db":"2026-09-17T00:00:00Z",
		"counts":{"critical":1,"high":2},
		"findings":["CRITICAL:CVE-2026-0001","HIGH:GHSA-abcd-efgh","BOGUS:CVE-1","HIGH:","HIGH:a b"]}`), &report)
	if err != nil {
		t.Fatal(err)
	}
	want := []scan.Finding{
		{Severity: scan.SeverityCritical, ID: "CVE-2026-0001"},
		{Severity: scan.SeverityHigh, ID: "GHSA-abcd-efgh"},
	}
	if diff := cmp.Diff(want, report.Listed()); diff != "" {
		t.Errorf("listed (-want +got):\n%s", diff)
	}
	if got := report.Counts.AtLeast(scan.SeverityHigh); got != 3 {
		t.Errorf("at least high: got %d", got)
	}
	if got := report.Counts.AtLeast(scan.SeverityCritical); got != 1 {
		t.Errorf("at least critical: got %d", got)
	}
	var down scan.Report
	if err := json.Unmarshal([]byte(`{"status":"unavailable","detail":"no feed"}`), &down); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(scan.Unavailable("no feed"), down, optCmp); diff != "" {
		t.Errorf("unavailable (-want +got):\n%s", diff)
	}
}

func TestListedFindings(t *testing.T) {
	many := make([]string, 0, scan.MaxListed+5)
	for range scan.MaxListed + 5 {
		many = append(many, "low:CVE-1")
	}
	tests := []struct {
		name     string
		findings []string
		want     []scan.Finding
	}{
		{"none", nil, []scan.Finding{}},
		{"no separator", []string{"HIGH"}, []scan.Finding{}},
		{"id is trimmed", []string{"high:  CVE-1\t"}, []scan.Finding{{Severity: scan.SeverityHigh, ID: "CVE-1"}}},
		{"severity is not trimmed", []string{" high:CVE-1"}, []scan.Finding{}},
		{"id may hold colons", []string{"Medium:a:b_c.d-e"}, []scan.Finding{{Severity: scan.SeverityMedium, ID: "a:b_c.d-e"}}},
		{"id of 64 bytes", []string{"low:" + strings.Repeat("a", 64)}, []scan.Finding{{Severity: scan.SeverityLow, ID: strings.Repeat("a", 64)}}},
		{"id of 65 bytes", []string{"low:" + strings.Repeat("a", 65)}, []scan.Finding{}},
		{"non-ascii id", []string{"low:CVÉ-1"}, []scan.Finding{}},
		{"non-ascii case folding is not applied", []string{"crİtİcal:CVE-1"}, []scan.Finding{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scan.Report{Status: scan.StatusOK, Findings: tt.findings}.Listed()
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
	if got := len((scan.Report{Findings: many}).Listed()); got != scan.MaxListed {
		t.Errorf("listed %d findings, want %d", got, scan.MaxListed)
	}
}

func TestOpenFindingsAboveTheSeverityAreRefused(t *testing.T) {
	gate := scan.Production()
	critical := scan.Counts{Critical: 1, High: 4}
	findings := []scan.Finding{{Severity: scan.SeverityCritical, ID: "CVE-1"}, {Severity: scan.SeverityHigh, ID: "CVE-2"}}
	scans := one(opt.Some(summary(critical, findings, 60)))

	reason := blocked(t, scan.Evaluate(gate, scans, nil, now))
	if !strings.Contains(reason, "1 critical") || !strings.Contains(reason, "CVE-1") {
		t.Errorf("got %q", reason)
	}
	want := "sha256:0123456789ab… has 1 critical or worse finding(s) without an exception: CVE-1"
	if reason != want {
		t.Errorf("got %q, want %q", reason, want)
	}

	excepted := except("CVE-1")
	if got := scan.Evaluate(gate, scans, excepted, now); got != (scan.Pass{}) {
		t.Errorf("the critical finding has an exception; high ones do not count: got %#v", got)
	}

	high := gate
	high.Severity = scan.SeverityHigh
	if reason := blocked(t, scan.Evaluate(high, scans, excepted, now)); !strings.Contains(reason, "4 high") {
		t.Errorf("got %q", reason)
	}

	warn := gate
	warn.Mode = scan.ModeWarn
	if got, ok := scan.Evaluate(warn, scans, nil, now).(scan.Warn); !ok || len(got.Reasons) != 1 {
		t.Errorf("got %#v, want a warning", got)
	}
	if got := scan.Evaluate(scan.Off(), scans, nil, now); got != (scan.Pass{}) {
		t.Errorf("got %#v", got)
	}
	if got := scan.Evaluate(scan.Gate{}, scans, nil, now); got != (scan.Pass{}) {
		t.Errorf("the zero gate checks nothing: got %#v", got)
	}
}

func TestUnlistedFindingsCannotBeExcepted(t *testing.T) {
	// Two critical findings, only one of them listed and excepted.
	s := summary(scan.Counts{Critical: 2}, []scan.Finding{{Severity: scan.SeverityCritical, ID: "CVE-1"}}, 60)
	reason := blocked(t, scan.Evaluate(scan.Production(), one(opt.Some(s)), except("CVE-1"), now))
	want := "sha256:0123456789ab… has 1 critical or worse finding(s) without an exception"
	if reason != want {
		t.Errorf("got %q, want %q", reason, want)
	}
}

func TestMissingStaleOrUnavailableScansCountOnlyWhenRequired(t *testing.T) {
	gate := scan.Production()
	clean := summary(scan.Counts{}, nil, 60)
	stale := summary(scan.Counts{}, nil, int64(gate.MaxAgeSecs)+1)
	unavailable := clean
	unavailable.Status = scan.StatusUnavailable
	strict := gate
	strict.RequireScan = true

	tests := []struct {
		name string
		scan opt.Val[scan.Summary]
	}{
		{"missing", opt.None[scan.Summary]()},
		{"stale", opt.Some(stale)},
		{"unavailable", opt.Some(unavailable)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scan.Evaluate(gate, one(tt.scan), nil, now); got != (scan.Pass{}) {
				t.Errorf("got %#v", got)
			}
			reason := blocked(t, scan.Evaluate(strict, one(tt.scan), nil, now))
			if reason != "sha256:0123456789ab… has no fresh vulnerability scan" {
				t.Errorf("got %q", reason)
			}
		})
	}
	// A scan exactly as old as allowed still counts.
	edge := summary(scan.Counts{Critical: 1}, nil, int64(gate.MaxAgeSecs))
	blocked(t, scan.Evaluate(gate, one(opt.Some(edge)), nil, now))
	// A stale scan's findings do not count either: rescans refresh them.
	staleCritical := stale
	staleCritical.Counts = scan.Counts{Critical: 3}
	if got := scan.Evaluate(gate, one(opt.Some(staleCritical)), nil, now); got != (scan.Pass{}) {
		t.Errorf("got %#v", got)
	}
}

func TestGatesWeakenInEveryDirection(t *testing.T) {
	strict := scan.Gate{Mode: scan.ModeBlock, Severity: scan.SeverityHigh, RequireScan: true, MaxAgeSecs: 86_400}
	if strict.Weakens(strict) {
		t.Error("a gate does not weaken itself")
	}
	weaker := map[string]func(*scan.Gate){
		"mode":        func(g *scan.Gate) { g.Mode = scan.ModeWarn },
		"severity":    func(g *scan.Gate) { g.Severity = scan.SeverityCritical },
		"requireScan": func(g *scan.Gate) { g.RequireScan = false },
		"maxAgeSecs":  func(g *scan.Gate) { g.MaxAgeSecs = 172_800 },
	}
	for name, change := range weaker {
		w := strict
		change(&w)
		if !w.Weakens(strict) {
			t.Errorf("%s: %+v must weaken %+v", name, w, strict)
		}
		if strict.Weakens(w) {
			t.Errorf("%s: %+v must not weaken %+v", name, strict, w)
		}
	}
	if !strict.Valid() {
		t.Error("strict is valid")
	}
	invalid := map[string]func(*scan.Gate){
		"low severity":      func(g *scan.Gate) { g.Severity = scan.SeverityLow },
		"too young":         func(g *scan.Gate) { g.MaxAgeSecs = 10 },
		"almost old enough": func(g *scan.Gate) { g.MaxAgeSecs = 3599 },
		"too old":           func(g *scan.Gate) { g.MaxAgeSecs = 90*24*3600 + 1 },
		"unknown mode":      func(g *scan.Gate) { g.Mode = "" },
	}
	for name, change := range invalid {
		g := strict
		change(&g)
		if g.Valid() {
			t.Errorf("%s: %+v must be invalid", name, g)
		}
	}
	for _, age := range []uint32{3600, 90 * 24 * 3600} {
		g := strict
		g.MaxAgeSecs = age
		if !g.Valid() {
			t.Errorf("a maximum age of %d is valid", age)
		}
	}
	if !scan.Off().Valid() || !scan.Production().Valid() {
		t.Error("the built-in gates are valid")
	}
	if got, err := scan.ParseGateMode("block"); err != nil || got != scan.ModeBlock {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err := scan.ParseSeverity("HIGH"); err != nil || got != scan.SeverityHigh {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestParsing(t *testing.T) {
	for _, s := range []scan.Severity{scan.SeverityUnknown, scan.SeverityLow, scan.SeverityMedium, scan.SeverityHigh, scan.SeverityCritical} {
		for _, text := range []string{string(s), strings.ToUpper(string(s))} {
			if got, err := scan.ParseSeverity(text); err != nil || got != s {
				t.Errorf("severity %q: got %q, %v", text, got, err)
			}
		}
	}
	for _, m := range []scan.GateMode{scan.ModeOff, scan.ModeWarn, scan.ModeBlock} {
		if got, err := scan.ParseGateMode(string(m)); err != nil || got != m {
			t.Errorf("mode %q: got %q, %v", m, got, err)
		}
	}
	for _, s := range []scan.Status{scan.StatusOK, scan.StatusUnavailable} {
		if got, err := scan.ParseStatus(string(s)); err != nil || got != s {
			t.Errorf("status %q: got %q, %v", s, got, err)
		}
	}
	_, errSeverity := scan.ParseSeverity("severe")
	_, errMode := scan.ParseGateMode("BLOCK") // Gate modes are case-sensitive.
	_, errStatus := scan.ParseStatus("")
	for _, err := range []error{errSeverity, errMode, errStatus} {
		if !errors.Is(err, kerrors.ErrValidation) {
			t.Errorf("got %v, want a validation error", err)
		}
	}
}

func TestSeveritiesAndModesAreOrderedByRank(t *testing.T) {
	severities := []scan.Severity{scan.SeverityUnknown, scan.SeverityLow, scan.SeverityMedium, scan.SeverityHigh, scan.SeverityCritical}
	for i, a := range severities {
		for j, b := range severities {
			got := a.Compare(b)
			if (i < j) != (got < 0) || (i == j) != (got == 0) {
				t.Errorf("%s compared with %s: got %d", a, b, got)
			}
		}
	}
	// The strings sort differently: "critical" < "high" < "low".
	if scan.SeverityCritical.Compare(scan.SeverityLow) <= 0 {
		t.Error("critical is more severe than low")
	}
	if scan.Severity("bogus").Rank() != -1 || scan.GateMode("bogus").Rank() != -1 {
		t.Error("unknown values rank below everything")
	}
	if scan.ModeOff.Rank() >= scan.ModeWarn.Rank() || scan.ModeWarn.Rank() >= scan.ModeBlock.Rank() {
		t.Error("off < warn < block")
	}
}

func TestCountsAtLeast(t *testing.T) {
	c := scan.Counts{Critical: 1, High: 2, Medium: 4, Low: 8, Unknown: 16}
	tests := []struct {
		severity scan.Severity
		want     uint32
	}{
		{scan.SeverityCritical, 1},
		{scan.SeverityHigh, 3},
		{scan.SeverityMedium, 7},
		{scan.SeverityLow, 15},
		{scan.SeverityUnknown, 31},
		{scan.Severity("bogus"), 1},
	}
	for _, tt := range tests {
		if got := c.AtLeast(tt.severity); got != tt.want {
			t.Errorf("at least %s: got %d, want %d", tt.severity, got, tt.want)
		}
	}
	full := scan.Counts{Critical: math.MaxUint32, High: 1, Medium: math.MaxUint32}
	if got := full.AtLeast(scan.SeverityUnknown); got != math.MaxUint32 {
		t.Errorf("counts saturate: got %d", got)
	}
}

func TestEvaluateDoesNotOverflow(t *testing.T) {
	gate := scan.Production()
	gate.RequireScan = true
	tests := []struct {
		name      string
		now       int64
		scannedAt int64
		fresh     bool
	}{
		{"scanned at the beginning of time", math.MaxInt64, math.MinInt64, false},
		{"scanned far in the future", math.MinInt64, math.MaxInt64, true},
		{"now at the limit", math.MaxInt64, math.MaxInt64 - 1000, true},
		{"negative scan time", 1, math.MinInt64, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := scan.Summary{Status: scan.StatusOK, ScannedAt: tt.scannedAt}
			_, passed := scan.Evaluate(gate, one(opt.Some(s)), nil, tt.now).(scan.Pass)
			if passed != tt.fresh {
				t.Errorf("fresh: got %v, want %v", passed, tt.fresh)
			}
		})
	}
}

func TestReasons(t *testing.T) {
	gate := scan.Production()
	gate.Severity = scan.SeverityHigh
	var findings []scan.Finding
	for _, id := range []string{"A", "B", "C", "D", "E", "F", "G"} {
		findings = append(findings, scan.Finding{Severity: scan.SeverityHigh, ID: id})
	}
	findings = append(findings, scan.Finding{Severity: scan.SeverityMedium, ID: "M"})
	scans := []scan.Scanned{
		{Digest: "short", Scan: opt.Some(summary(scan.Counts{High: 7}, findings, 60))},
		{Digest: "sha256:0123456789aé0000", Scan: opt.Some(summary(scan.Counts{Critical: 1}, nil, 60))},
		{Digest: digest, Scan: opt.Some(summary(scan.Counts{Medium: 9}, nil, 60))},
	}
	want := scan.Block{Reasons: []string{
		"short… has 6 high or worse finding(s) without an exception: A, C, D, E, F",
		"sha256:0123456789aé0000… has 1 high or worse finding(s) without an exception",
	}}
	if diff := cmp.Diff(scan.Verdict(want), scan.Evaluate(gate, scans, except("B", "M"), now)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestWireFormat(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"severity", scan.SeverityCritical, `"critical"`},
		{"status", scan.StatusUnavailable, `"unavailable"`},
		{"gate mode", scan.ModeWarn, `"warn"`},
		{"counts", scan.Counts{Critical: 1, Unknown: 2}, `{"critical":1,"high":0,"medium":0,"low":0,"unknown":2}`},
		{"off gate", scan.Off(), `{"mode":"off","severity":"critical","requireScan":false,"maxAgeSecs":604800}`},
		{"production gate", scan.Production(), `{"mode":"block","severity":"critical","requireScan":false,"maxAgeSecs":604800}`},
		{
			"unavailable report", scan.Unavailable("no feed"),
			`{"status":"unavailable","scanner":"","db":null,"counts":{"critical":0,"high":0,"medium":0,"low":0,"unknown":0},"findings":[],"detail":"no feed"}`,
		},
		{
			"report without findings",
			scan.Report{Status: scan.StatusOK, Scanner: "trivy", DB: opt.Some("2026-09-17T00:00:00Z")},
			`{"status":"ok","scanner":"trivy","db":"2026-09-17T00:00:00Z","counts":{"critical":0,"high":0,"medium":0,"low":0,"unknown":0},"findings":[],"detail":null}`,
		},
		{
			"report with findings",
			scan.Report{Status: scan.StatusOK, Counts: scan.Counts{High: 1}, Findings: []string{"HIGH:CVE-1"}},
			`{"status":"ok","scanner":"","db":null,"counts":{"critical":0,"high":1,"medium":0,"low":0,"unknown":0},"findings":["HIGH:CVE-1"],"detail":null}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestReportsAndGatesRoundTrip(t *testing.T) {
	report := scan.Report{
		Status: scan.StatusOK, Scanner: "trivy 0.74.0", DB: opt.Some("2026-09-17T00:00:00Z"),
		Counts: scan.Counts{Critical: 1, High: 2}, Findings: []string{"CRITICAL:CVE-1"},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var back scan.Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(report, back, optCmp); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	gate := scan.Gate{Mode: scan.ModeWarn, Severity: scan.SeverityHigh, RequireScan: true, MaxAgeSecs: 3600}
	data, err = json.Marshal(gate)
	if err != nil {
		t.Fatal(err)
	}
	var gateBack scan.Gate
	if err := json.Unmarshal(data, &gateBack); err != nil {
		t.Fatal(err)
	}
	if gateBack != gate {
		t.Errorf("got %+v, want %+v", gateBack, gate)
	}
}

func TestDecodingRefusesWhatSerdeRefuses(t *testing.T) {
	tests := []struct {
		name   string
		target any
		input  string
	}{
		{"report without status", new(scan.Report), `{"scanner":"trivy"}`},
		{"report with unknown status", new(scan.Report), `{"status":"fine"}`},
		{"report with upper-case status", new(scan.Report), `{"status":"OK"}`},
		{"upper-case severity", new(scan.Severity), `"HIGH"`},
		{"unknown severity", new(scan.Severity), `"severe"`},
		{"severity of the wrong type", new(scan.Severity), `3`},
		{"unknown gate mode", new(scan.GateMode), `"audit"`},
		{"gate without mode", new(scan.Gate), `{"severity":"high","requireScan":true,"maxAgeSecs":3600}`},
		{"gate without severity", new(scan.Gate), `{"mode":"off","requireScan":true,"maxAgeSecs":3600}`},
		{"gate without requireScan", new(scan.Gate), `{"mode":"off","severity":"high","maxAgeSecs":3600}`},
		{"gate without maxAgeSecs", new(scan.Gate), `{"mode":"off","severity":"high","requireScan":true}`},
		{"gate with a negative age", new(scan.Gate), `{"mode":"off","severity":"high","requireScan":true,"maxAgeSecs":-1}`},
		{"gate with snake-case fields", new(scan.Gate), `{"mode":"off","severity":"high","require_scan":true,"max_age_secs":3600}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.input), tt.target); err == nil {
				t.Errorf("decoded %s into %+v", tt.input, tt.target)
			}
		})
	}
}
