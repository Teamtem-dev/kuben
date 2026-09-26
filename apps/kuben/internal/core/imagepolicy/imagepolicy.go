// Package imagepolicy has image update policies (M5.4; plan §20.2): which
// tag of a repository an app follows, and how often to look. It replaces
// the Rust module kuben-core/src/image_policy.rs.
//
// A policy follows one of:
//   - a SemVer range (`semver:^1.2`, `semver:1.2.*`, `semver:>=1.0, <2`): the
//     highest release matching it, prereleases only when the range names one;
//   - a tag glob (`tag:release-*`): the highest matching tag by version-aware
//     order;
//   - one tag (`latest`, or any other name): its current digest.
//
// The registry is asked at most every interval, and less often after
// failures (exponential backoff up to a day).
package imagepolicy

import (
	"cmp"
	"errors"
	"math"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// MinIntervalSecs is the shortest interval between checks.
const MinIntervalSecs uint32 = 60

// MaxIntervalSecs is the longest interval between checks, and the
// backoff's ceiling.
const MaxIntervalSecs uint32 = 86_400

// Pattern is what a policy follows: a [Semver] range, a tag [Glob] or one
// [Tag].
//
//sumtype:decl
type Pattern interface {
	// Text is the pattern as stored and shown.
	Text() string
	// Select is the tag to follow among tags, if any matches. A plain tag
	// needs no listing: it is itself.
	Select(tags []string) opt.Val[string]
	// NeedsListing reports whether the registry must list tags for this
	// pattern.
	NeedsListing() bool
	pattern()
}

// Semver follows the highest release in a SemVer range.
type Semver struct{ Req VersionReq }

// Glob follows the highest tag matching a glob, where `*` is any run of
// characters.
type Glob struct{ Glob string }

// Tag follows one tag.
type Tag struct{ Tag string }

func (Semver) pattern() {}
func (Glob) pattern()   {}
func (Tag) pattern()    {}

// PatternErrorKind says why a pattern is not one.
type PatternErrorKind string

// The kinds.
const (
	// BadRange: the text after `semver:` is not a SemVer range.
	BadRange PatternErrorKind = "range"
	// BadTag: not a valid tag or tag glob.
	BadTag PatternErrorKind = "tag"
)

// PatternError is a refused pattern.
type PatternError struct {
	Kind PatternErrorKind
	// Text is the range or tag that was refused, without its prefix.
	Text string
	// Reason is what the range parser said (BadRange only).
	Reason string
}

func (e *PatternError) Error() string {
	if e.Kind == BadRange {
		return "`" + e.Text + "` is not a SemVer range: " + e.Reason
	}
	return "`" + e.Text + "` is not a valid tag or tag glob"
}

// Unwrap makes the error a validation failure for errors.Is and kerrors.CodeOf.
func (e *PatternError) Unwrap() error {
	return kerrors.New(kerrors.Validation, "%s", e.Error())
}

func validTag(tag string, glob bool) bool {
	if tag == "" || len(tag) > 128 || tag[0] == '.' || tag[0] == '-' {
		return false
	}
	for i := range len(tag) {
		b := tag[i]
		if !isIdentByte(b) && b != '_' && b != '.' && (!glob || b != '*') {
			return false
		}
	}
	return true
}

// Parse reads `semver:<range>`, `tag:<glob>` or a plain tag. A refusal is a
// *PatternError.
func Parse(text string) (Pattern, error) {
	text = strings.TrimSpace(text)
	if r, ok := strings.CutPrefix(text, "semver:"); ok {
		req, err := parseVersionReq(strings.TrimSpace(r))
		if err != nil {
			return nil, &PatternError{Kind: BadRange, Text: r, Reason: err.Error()}
		}
		return Semver{Req: req}, nil
	}
	if glob, ok := strings.CutPrefix(text, "tag:"); ok {
		if !validTag(glob, true) {
			return nil, &PatternError{Kind: BadTag, Text: glob}
		}
		return Glob{Glob: glob}, nil
	}
	if !validTag(text, false) {
		return nil, &PatternError{Kind: BadTag, Text: text}
	}
	return Tag{Tag: text}, nil
}

// Text is `semver:` and the normalized range.
func (p Semver) Text() string { return "semver:" + p.Req.String() }

// Text is `tag:` and the glob.
func (p Glob) Text() string { return "tag:" + p.Glob }

// Text is the tag.
func (p Tag) Text() string { return p.Tag }

// NeedsListing is true: the range is matched against the listed tags.
func (Semver) NeedsListing() bool { return true }

// NeedsListing is true: the glob is matched against the listed tags.
func (Glob) NeedsListing() bool { return true }

// NeedsListing is false: a plain tag is asked for directly.
func (Tag) NeedsListing() bool { return false }

// Select is the tag itself, whatever the registry lists.
func (p Tag) Select([]string) opt.Val[string] { return opt.Some(p.Tag) }

// Select is the tag with the highest version in the range; among equal
// versions (`1.2.0`, `v1.2.0`, `1.2`) the last one listed.
func (p Semver) Select(tags []string) opt.Val[string] {
	wantsPre := p.Req.WantsPrerelease()
	var best *semver.Version
	chosen := opt.None[string]()
	for _, tag := range tags {
		v, err := tagVersion(tag)
		if err != nil || (!wantsPre && v.Prerelease() != "") || !p.Req.Matches(v) {
			continue
		}
		if best == nil || compareVersions(v, best) >= 0 {
			best, chosen = v, opt.Some(tag)
		}
	}
	return chosen
}

// Select is the highest matching tag in natural order; among equals the
// last one listed.
func (p Glob) Select(tags []string) opt.Val[string] {
	chosen := opt.None[string]()
	for _, tag := range tags {
		if !globMatches(p.Glob, tag) {
			continue
		}
		if best, ok := chosen.Get(); !ok || natural(tag, best) >= 0 {
			chosen = opt.Some(tag)
		}
	}
	return chosen
}

// errNotVersion: a tag that is no version.
var errNotVersion = errors.New("not a version")

// tagVersion reads a tag as a version: `1.2.3`, `v1.2.3`, `1.2` (as
// `1.2.0`), `1` (as `1.0.0`). Parsing is strict SemVer 2.0, as in Rust;
// Masterminds' lenient NewVersion is deliberately not used.
func tagVersion(tag string) (*semver.Version, error) {
	t := strings.TrimPrefix(tag, "v")
	if v, err := semver.StrictNewVersion(t); err == nil {
		return v, nil
	}
	switch strings.Count(t, ".") {
	case 1:
		t += ".0"
	case 0:
		if !allDigits(t) {
			return nil, errNotVersion
		}
		t += ".0.0"
	default:
		return nil, errNotVersion
	}
	return semver.StrictNewVersion(t)
}

// globMatches matches tag against glob, where `*` is any run of characters
// (also none) and every other character is itself.
func globMatches(glob, tag string) bool {
	parts := strings.Split(glob, "*")
	if len(parts) == 1 {
		return glob == tag
	}
	first, last := parts[0], parts[len(parts)-1]
	if !strings.HasPrefix(tag, first) || !strings.HasSuffix(tag, last) || len(tag) < len(first)+len(last) {
		return false
	}
	rest := tag[len(first) : len(tag)-len(last)]
	for _, middle := range parts[1 : len(parts)-1] {
		at := strings.Index(rest, middle)
		if at < 0 {
			return false
		}
		rest = rest[at+len(middle):]
	}
	return true
}

// natural orders text with digit runs compared as numbers: `r-10` after
// `r-9`.
func natural(a, b string) int {
	x, y := chunks(a), chunks(b)
	for i := range min(len(x), len(y)) {
		var order int
		if x[i].digits && y[i].digits {
			tx, ty := strings.TrimLeft(x[i].run, "0"), strings.TrimLeft(y[i].run, "0")
			order = cmp.Or(cmp.Compare(len(tx), len(ty)), strings.Compare(tx, ty))
		} else {
			order = strings.Compare(x[i].run, y[i].run)
		}
		if order != 0 {
			return order
		}
	}
	return cmp.Compare(len(x), len(y))
}

type chunk struct {
	digits bool
	run    string
}

// chunks splits s into runs of ASCII digits and runs of everything else.
func chunks(s string) []chunk {
	out := make([]chunk, 0, 4)
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || isDigit(s[i]) != isDigit(s[start]) {
			out = append(out, chunk{digits: isDigit(s[start]), run: s[start:i]})
			start = i
		}
	}
	return out
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// NextCheck is when to look again after failures failures in a row, with
// intervalSecs between successful checks (or retryAfter seconds asked by
// the registry, when that is longer). Times are unix milliseconds.
func NextCheck(now int64, intervalSecs, failures uint32, retryAfter opt.Val[uint64]) int64 {
	base := uint64(min(max(intervalSecs, MinIntervalSecs), MaxIntervalSecs))
	// At most 86 400 << 16: far from overflowing.
	wait := min(base<<min(failures, 16), uint64(MaxIntervalSecs))
	if r, ok := retryAfter.Get(); ok {
		wait = max(wait, r)
	}
	if wait > math.MaxInt64/1000 {
		return math.MaxInt64
	}
	return clock.SaturatingAdd(now, int64(wait)*1000)
}
