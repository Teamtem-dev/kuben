// Package scan has image SBOMs and vulnerability scans (M4.6; plan §13.3,
// §18.1). It replaces the Rust module kuben-core/src/scan.rs.
//
// Every built image is scanned by digest; the scan records the scanner and
// the age of its vulnerability database, because a scan is only as fresh as
// its feed and an unavailable feed is never "clean". An environment's gate
// decides what a deployment may carry: nothing (off), a warning, or a
// refusal of findings at or above a severity that no unexpired exception
// covers, and optionally of images without a fresh scan. Provenance,
// signatures and findings are separate questions; this package answers only
// the last.
package scan

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

const (
	// MaxListed is how many findings a scan lists by id, at most.
	MaxListed = 40
	// MaxExceptionSecs is the longest exception a person may grant.
	MaxExceptionSecs int64 = 90 * 24 * 3600
	// DefaultMaxAgeSecs is the default age after which a scan no longer counts.
	DefaultMaxAgeSecs uint32 = 7 * 24 * 3600

	minMaxAgeSecs uint32 = 3600
	maxMaxAgeSecs uint32 = 90 * 24 * 3600
	// shortDigest is how much of a digest a reason shows: "sha256:" and
	// twelve hex digits.
	shortDigest = 19
	// maxFindingID is the longest finding id that is listed.
	maxFindingID = 64
	// reasonIDs is how many open finding ids a reason names.
	reasonIDs = 5
)

// Severity is a finding's severity, as scanners report it. Severities are
// ordered by [Severity.Rank], never by their strings.
type Severity string

// The severities, from least to most severe.
const (
	SeverityUnknown  Severity = "unknown"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// ParseSeverity reads a severity name in any ASCII case, as scanners write
// them ("HIGH", "high").
func ParseSeverity(s string) (Severity, error) {
	switch v := Severity(ascii.Lower(s)); v {
	case SeverityUnknown, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return v, nil
	}
	return "", kerrors.New(kerrors.Validation, "unknown severity `%s`", s)
}

// Rank is the severity's position in the order unknown < low < medium <
// high < critical; -1 for a value that is not a severity.
func (s Severity) Rank() int {
	switch s {
	case SeverityUnknown:
		return 0
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	case SeverityHigh:
		return 3
	case SeverityCritical:
		return 4
	}
	return -1
}

// Compare is negative when s is less severe than other, zero when they are
// equally severe and positive when s is more severe.
func (s Severity) Compare(other Severity) int { return s.Rank() - other.Rank() }

func (s Severity) String() string { return string(s) }

// UnmarshalJSON accepts only the exact wire names.
func (s *Severity) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	v := Severity(text)
	if v.Rank() < 0 {
		return kerrors.New(kerrors.Validation, "unknown severity `%s`", text)
	}
	*s = v
	return nil
}

// Counts are the findings per severity.
type Counts struct {
	Critical uint32 `json:"critical"`
	High     uint32 `json:"high"`
	Medium   uint32 `json:"medium"`
	Low      uint32 `json:"low"`
	Unknown  uint32 `json:"unknown"`
}

// AtLeast is the number of findings at severity or above, saturating. A
// value that is not a severity counts critical findings only.
func (c Counts) AtLeast(severity Severity) uint32 {
	n := c.Critical
	rank := severity.Rank()
	if rank < 0 {
		return n
	}
	for _, level := range []struct {
		rank  int
		count uint32
	}{
		{SeverityHigh.Rank(), c.High},
		{SeverityMedium.Rank(), c.Medium},
		{SeverityLow.Rank(), c.Low},
		{SeverityUnknown.Rank(), c.Unknown},
	} {
		if rank <= level.rank {
			n = saturatingAddU32(n, level.count)
		}
	}
	return n
}

// Status says whether a scan could run.
type Status string

// The scan statuses.
const (
	StatusOK Status = "ok"
	// StatusUnavailable: the scanner, its feed or the image was not available.
	StatusUnavailable Status = "unavailable"
)

// ParseStatus reads a scan status.
func ParseStatus(s string) (Status, error) {
	switch v := Status(s); v {
	case StatusOK, StatusUnavailable:
		return v, nil
	}
	return "", kerrors.New(kerrors.Validation, "unknown scan status `%s`", s)
}

func (s Status) String() string { return string(s) }

// UnmarshalJSON accepts only the exact wire names.
func (s *Status) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	v, err := ParseStatus(text)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// Finding is one finding by id.
type Finding struct {
	Severity Severity
	ID       string
}

// Report is what a scan container writes to its termination log (at most
// 4 KiB).
type Report struct {
	Status  Status `json:"status"`
	Scanner string `json:"scanner"`
	// DB is when the vulnerability database was built (RFC 3339).
	DB     opt.Val[string] `json:"db"`
	Counts Counts          `json:"counts"`
	// Findings are `SEVERITY:ID`, most severe first.
	Findings []string        `json:"findings"`
	Detail   opt.Val[string] `json:"detail"`
}

// Unavailable is the report of a scan that could not run.
func Unavailable(detail string) Report {
	return Report{Status: StatusUnavailable, Findings: []string{}, Detail: opt.Some(detail)}
}

// MarshalJSON writes no findings as `[]`, never as null.
func (r Report) MarshalJSON() ([]byte, error) {
	type wire Report
	w := wire(r)
	if w.Findings == nil {
		w.Findings = []string{}
	}
	return json.Marshal(w)
}

// UnmarshalJSON requires the status; everything else defaults to empty.
func (r *Report) UnmarshalJSON(data []byte) error {
	type wire Report
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.Status == "" {
		return kerrors.New(kerrors.Validation, "missing field `status`")
	}
	if w.Findings == nil {
		w.Findings = []string{}
	}
	*r = Report(w)
	return nil
}

// Listed are the listed findings that parse, at most [MaxListed].
func (r Report) Listed() []Finding {
	listed := []Finding{}
	for _, f := range r.Findings {
		if len(listed) == MaxListed {
			break
		}
		name, id, found := strings.Cut(f, ":")
		if !found {
			continue
		}
		id = strings.TrimSpace(id)
		if !validFindingID(id) {
			continue
		}
		severity, err := ParseSeverity(name)
		if err != nil {
			continue
		}
		listed = append(listed, Finding{Severity: severity, ID: id})
	}
	return listed
}

func validFindingID(id string) bool {
	if id == "" || len(id) > maxFindingID {
		return false
	}
	for i := range len(id) {
		b := id[i]
		alnum := b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
		if !alnum && b != '-' && b != '_' && b != '.' && b != ':' {
			return false
		}
	}
	return true
}

// Summary is a recorded scan of one digest.
type Summary struct {
	Status  Status
	Scanner string
	// DBUpdatedAt is when the vulnerability database was built (unix ms).
	DBUpdatedAt opt.Val[int64]
	Counts      Counts
	Findings    []Finding
	// ScannedAt is when the scan ran (unix ms).
	ScannedAt int64
}

// GateMode is what an environment's gate does. Modes are ordered by
// [GateMode.Rank]: off < warn < block.
type GateMode string

// The gate modes, from weakest to strictest.
const (
	ModeOff   GateMode = "off"
	ModeWarn  GateMode = "warn"
	ModeBlock GateMode = "block"
)

// ParseGateMode reads a gate mode.
func ParseGateMode(s string) (GateMode, error) {
	switch v := GateMode(s); v {
	case ModeOff, ModeWarn, ModeBlock:
		return v, nil
	}
	return "", kerrors.New(kerrors.Validation, "unknown gate mode `%s`", s)
}

// Rank is the mode's strictness; -1 for a value that is not a mode.
func (m GateMode) Rank() int {
	switch m {
	case ModeOff:
		return 0
	case ModeWarn:
		return 1
	case ModeBlock:
		return 2
	}
	return -1
}

func (m GateMode) String() string { return string(m) }

// UnmarshalJSON accepts only the exact wire names.
func (m *GateMode) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	v, err := ParseGateMode(text)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// Gate is an environment's vulnerability gate. The zero Gate is not a valid
// gate; [Off] is the default one.
type Gate struct {
	Mode GateMode `json:"mode"`
	// Severity: findings at or above it count (`high` or `critical`).
	Severity Severity `json:"severity"`
	// RequireScan: an image without a fresh, successful scan counts as a
	// finding.
	RequireScan bool `json:"requireScan"`
	// MaxAgeSecs: a scan older than this no longer counts.
	MaxAgeSecs uint32 `json:"maxAgeSecs"`
}

// Off is the default gate: nothing is checked.
func Off() Gate {
	return Gate{Mode: ModeOff, Severity: SeverityCritical, RequireScan: false, MaxAgeSecs: DefaultMaxAgeSecs}
}

// Production is the gate of a production environment: known critical
// findings are refused.
func Production() Gate {
	g := Off()
	g.Mode = ModeBlock
	return g
}

// UnmarshalJSON requires every field, as the stored and the API form do.
func (g *Gate) UnmarshalJSON(data []byte) error {
	var w struct {
		Mode        opt.Val[GateMode] `json:"mode"`
		Severity    opt.Val[Severity] `json:"severity"`
		RequireScan opt.Val[bool]     `json:"requireScan"`
		MaxAgeSecs  opt.Val[uint32]   `json:"maxAgeSecs"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	mode, hasMode := w.Mode.Get()
	severity, hasSeverity := w.Severity.Get()
	requireScan, hasRequireScan := w.RequireScan.Get()
	maxAge, hasMaxAge := w.MaxAgeSecs.Get()
	for _, field := range []struct {
		name    string
		present bool
	}{{"mode", hasMode}, {"severity", hasSeverity}, {"requireScan", hasRequireScan}, {"maxAgeSecs", hasMaxAge}} {
		if !field.present {
			return kerrors.New(kerrors.Validation, "missing field `%s`", field.name)
		}
	}
	*g = Gate{Mode: mode, Severity: severity, RequireScan: requireScan, MaxAgeSecs: maxAge}
	return nil
}

// Valid reports whether the gate may be stored: a known mode, a severity of
// `high` or `critical`, and a maximum age of an hour to 90 days.
func (g Gate) Valid() bool {
	return g.Mode.Rank() >= 0 &&
		(g.Severity == SeverityHigh || g.Severity == SeverityCritical) &&
		g.MaxAgeSecs >= minMaxAgeSecs && g.MaxAgeSecs <= maxMaxAgeSecs
}

// Weakens reports whether g refuses less than current.
func (g Gate) Weakens(current Gate) bool {
	return g.Mode.Rank() < current.Mode.Rank() ||
		g.Severity.Compare(current.Severity) > 0 ||
		(current.RequireScan && !g.RequireScan) ||
		g.MaxAgeSecs > current.MaxAgeSecs
}

// Verdict is what the gate decided for a deployment.
//
//sumtype:decl
type Verdict interface{ verdict() }

// Pass means the deployment may go ahead.
type Pass struct{}

// Warn means the deployment may go ahead, with these reasons shown.
type Warn struct{ Reasons []string }

// Block means the deployment is refused for these reasons.
type Block struct{ Reasons []string }

func (Pass) verdict()  {}
func (Warn) verdict()  {}
func (Block) verdict() {}

// Scanned is an image of a deployment: its digest and its newest scan, if
// it has one.
type Scanned struct {
	Digest string
	Scan   opt.Val[Summary]
}

// Evaluate decides on the images scans under gate at now (unix ms); excepted
// are the finding ids with an unexpired exception. A gate whose mode is not
// a known mode checks nothing, like [ModeOff].
func Evaluate(gate Gate, scans []Scanned, excepted map[string]struct{}, now int64) Verdict {
	if gate.Mode != ModeWarn && gate.Mode != ModeBlock {
		return Pass{}
	}
	var reasons []string
	for _, image := range scans {
		short := shorten(image.Digest)
		scan, ok := image.Scan.Get()
		fresh := ok && scan.Status == StatusOK &&
			clock.SaturatingSub(now, scan.ScannedAt) <= int64(gate.MaxAgeSecs)*1000
		if !fresh {
			if gate.RequireScan {
				reasons = append(reasons, short+"… has no fresh vulnerability scan")
			}
			continue
		}
		if reason, open := openFindings(gate, scan, excepted); open {
			reasons = append(reasons, short+reason)
		}
	}
	switch {
	case len(reasons) == 0:
		return Pass{}
	case gate.Mode == ModeWarn:
		return Warn{Reasons: reasons}
	default:
		return Block{Reasons: reasons}
	}
}

// openFindings is the reason (without the digest) a fresh scan counts
// against the gate, if it does. Findings that are counted but not listed
// cannot be excepted.
func openFindings(gate Gate, scan Summary, excepted map[string]struct{}) (string, bool) {
	counted := scan.Counts.AtLeast(gate.Severity)
	var covered uint32
	var ids []string
	for _, f := range scan.Findings {
		if f.Severity.Compare(gate.Severity) < 0 {
			continue
		}
		if _, ok := excepted[f.ID]; ok {
			if covered < math.MaxUint32 {
				covered++
			}
		} else if len(ids) < reasonIDs {
			ids = append(ids, f.ID)
		}
	}
	if covered >= counted {
		return "", false
	}
	reason := fmt.Sprintf("… has %d %s or worse finding(s) without an exception", counted-covered, gate.Severity)
	if len(ids) > 0 {
		reason += ": " + strings.Join(ids, ", ")
	}
	return reason, true
}

// shorten is the first [shortDigest] bytes of digest, or all of it when it
// is shorter or that would split a character.
func shorten(digest string) string {
	if len(digest) < shortDigest {
		return digest
	}
	if len(digest) > shortDigest && !utf8.RuneStart(digest[shortDigest]) {
		return digest
	}
	return digest[:shortDigest]
}

func saturatingAddU32(a, b uint32) uint32 {
	if a > math.MaxUint32-b {
		return math.MaxUint32
	}
	return a + b
}
