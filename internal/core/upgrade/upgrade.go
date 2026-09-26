// Package upgrade says which version steps are supported, and what must
// hold before one starts (M4.8; plan §17.4, S07). It replaces the Rust
// module kuben-core/src/upgrade.rs.
//
// Kuben upgrades one minor version at a time (patch releases in between do
// not count) and never goes back: a database a newer Kuben migrated is not
// run by an older one. A major step is allowed from the newest minor of the
// previous major only after its notes were read, so it needs `--major`.
// The preflight is a list of findings; any failure stops the upgrade, and
// nothing has changed by then.
package upgrade

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Version is a release version, `MAJOR.MINOR.PATCH[-PRE]`.
type Version struct {
	Major uint64
	Minor uint64
	Patch uint64
	Pre   opt.Val[string]
}

// ParseVersion reads `v1.2.3`, `1.2.3` or `1.2.3-rc.1` (build metadata is
// ignored); false when s is not a version.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimLeft(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	if hasPre && pre == "" {
		return Version{}, false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var nums [3]uint64
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return Version{}, false
		}
		nums[i] = n
	}
	v := Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}
	if hasPre {
		v.Pre = opt.Some(pre)
	}
	return v, true
}

// Compare orders versions: by number, then a pre-release before its
// release, then pre-releases as plain text.
func (v Version) Compare(o Version) int {
	if c := cmp.Or(cmp.Compare(v.Major, o.Major), cmp.Compare(v.Minor, o.Minor), cmp.Compare(v.Patch, o.Patch)); c != 0 {
		return c
	}
	a, aPre := v.Pre.Get()
	b, bPre := o.Pre.Get()
	switch {
	case aPre && bPre:
		return strings.Compare(a, b)
	case aPre:
		return -1
	case bPre:
		return 1
	default:
		return 0
	}
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if pre, ok := v.Pre.Get(); ok {
		s += "-" + pre
	}
	return s
}

// Step is the kind of step an upgrade is. The strings appear in the
// preflight's version finding.
type Step string

// The steps.
const (
	// Same is the same version: a restart or a repair.
	Same  Step = "Same"
	Patch Step = "Patch"
	Minor Step = "Minor"
	Major Step = "Major"
)

// ParseStep reads a step name.
func ParseStep(s string) (Step, error) {
	switch st := Step(s); st {
	case Same, Patch, Minor, Major:
		return st, nil
	}
	return "", kerrors.New(kerrors.Validation, "unknown upgrade step `%s`", s)
}

// PathErrorKind says why a step is refused.
type PathErrorKind string

// The kinds.
const (
	// InvalidVersion: one of the two versions does not parse.
	InvalidVersion PathErrorKind = "invalid"
	// Downgrade: the target is older than what ran.
	Downgrade PathErrorKind = "downgrade"
	// SkipsMinor: more than one minor version ahead.
	SkipsMinor PathErrorKind = "skipsMinor"
	// MajorNeedsConsent: a major step without the operator's `--major`.
	MajorNeedsConsent PathErrorKind = "majorNeedsConsent"
	// SkipsMajor: more than one major version ahead.
	SkipsMajor PathErrorKind = "skipsMajor"
)

// PathError is a refused step.
type PathError struct {
	Kind PathErrorKind
	// Input is the text that is not a version (InvalidVersion only).
	Input string
	// From and To are the parsed versions (every other kind).
	From, To Version
	// FromMajor and NextMinor name the version to go to first (SkipsMinor).
	FromMajor, NextMinor uint64
}

func (e *PathError) Error() string {
	switch e.Kind {
	case InvalidVersion:
		return "`" + e.Input + "` is not a version"
	case Downgrade:
		return fmt.Sprintf("%s → %s goes back: a newer Kuben's database is not run by an older one", e.From, e.To)
	case SkipsMinor:
		return fmt.Sprintf("%s → %s skips minor versions: upgrade to %d.%d first", e.From, e.To, e.FromMajor, e.NextMinor)
	case MajorNeedsConsent:
		return fmt.Sprintf("%s → %s is a major upgrade: read its release notes, then pass --major", e.From, e.To)
	case SkipsMajor:
		return fmt.Sprintf("%s → %s skips a major version", e.From, e.To)
	}
	return fmt.Sprintf("%s → %s is not supported", e.From, e.To)
}

// Unwrap makes the error a validation failure for errors.Is and kerrors.CodeOf.
func (e *PathError) Unwrap() error {
	return kerrors.New(kerrors.Validation, "%s", e.Error())
}

// StepBetween says whether going from from to to is supported; major is the
// operator's consent to a major step. A refusal is a *PathError.
func StepBetween(from, to string, major bool) (Step, error) {
	f, ok := ParseVersion(from)
	if !ok {
		return "", &PathError{Kind: InvalidVersion, Input: from}
	}
	t, ok := ParseVersion(to)
	if !ok {
		return "", &PathError{Kind: InvalidVersion, Input: to}
	}
	switch c := t.Compare(f); {
	case c < 0:
		return "", &PathError{Kind: Downgrade, From: f, To: t}
	case c == 0:
		return Same, nil
	}
	if t.Major == f.Major {
		switch t.Minor - f.Minor {
		case 0:
			return Patch, nil
		case 1:
			return Minor, nil
		}
		return "", &PathError{Kind: SkipsMinor, From: f, To: t, FromMajor: f.Major, NextMinor: f.Minor + 1}
	}
	if t.Major-f.Major > 1 {
		return "", &PathError{Kind: SkipsMajor, From: f, To: t}
	}
	if !major {
		return "", &PathError{Kind: MajorNeedsConsent, From: f, To: t}
	}
	return Major, nil
}
