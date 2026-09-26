package imagepolicy

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Masterminds/semver/v3"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// The range grammar, its printed form and its matching are those of Rust's
// `semver` crate (Cargo's dialect), which wrote the patterns stored in
// existing installations. Masterminds/semver reads ranges differently (a
// bare `1.2.3` is exact there and a caret here, spaces separate comparators,
// `||` exists, pre-releases match more widely), so it parses the versions of
// tags only; ranges are parsed and evaluated here.

type op int

const (
	opExact op = iota
	opGreater
	opGreaterEq
	opLess
	opLessEq
	opTilde
	opCaret
	opWildcard
)

func (o op) String() string {
	switch o {
	case opExact:
		return "="
	case opGreater:
		return ">"
	case opGreaterEq:
		return ">="
	case opLess:
		return "<"
	case opLessEq:
		return "<="
	case opTilde:
		return "~"
	case opCaret:
		return "^"
	case opWildcard:
		return ""
	}
	return ""
}

type comparator struct {
	op    op
	major uint64
	minor opt.Val[uint64]
	patch opt.Val[uint64]
	pre   string
}

func (c comparator) String() string {
	s := c.op.String() + strconv.FormatUint(c.major, 10)
	minor, hasMinor := c.minor.Get()
	if !hasMinor {
		if c.op == opWildcard {
			s += ".*"
		}
		return s
	}
	s += "." + strconv.FormatUint(minor, 10)
	patch, hasPatch := c.patch.Get()
	if !hasPatch {
		if c.op == opWildcard {
			s += ".*"
		}
		return s
	}
	s += "." + strconv.FormatUint(patch, 10)
	if c.pre != "" {
		s += "-" + c.pre
	}
	return s
}

// VersionReq is a SemVer range: comparators that must all hold. The zero
// value is `*`, which every release matches.
type VersionReq struct {
	comparators []comparator
}

// String is the normalized range, as patterns are stored and shown:
// `^1.2`, `1.2.*`, `>=1.0, <2`, `*`.
func (r VersionReq) String() string {
	if len(r.comparators) == 0 {
		return "*"
	}
	parts := make([]string, len(r.comparators))
	for i, c := range r.comparators {
		parts[i] = c.String()
	}
	return strings.Join(parts, ", ")
}

// WantsPrerelease reports whether a comparator names a pre-release.
func (r VersionReq) WantsPrerelease() bool {
	for _, c := range r.comparators {
		if c.pre != "" {
			return true
		}
	}
	return false
}

// Matches reports whether v satisfies every comparator. A pre-release
// matches only when a comparator with the same major.minor.patch names a
// pre-release too.
func (r VersionReq) Matches(v *semver.Version) bool {
	if v == nil {
		return false
	}
	for _, c := range r.comparators {
		if !c.matches(v) {
			return false
		}
	}
	if v.Prerelease() == "" {
		return true
	}
	for _, c := range r.comparators {
		if c.preCompatible(v) {
			return true
		}
	}
	return false
}

func (c comparator) preCompatible(v *semver.Version) bool {
	return c.major == v.Major() && c.minor == opt.Some(v.Minor()) && c.patch == opt.Some(v.Patch()) && c.pre != ""
}

func (c comparator) matches(v *semver.Version) bool {
	switch c.op {
	case opExact, opWildcard:
		return c.exact(v)
	case opGreater:
		return c.ordered(v, 1)
	case opGreaterEq:
		return c.exact(v) || c.ordered(v, 1)
	case opLess:
		return c.ordered(v, -1)
	case opLessEq:
		return c.exact(v) || c.ordered(v, -1)
	case opTilde:
		return c.tilde(v)
	case opCaret:
		return c.caret(v)
	}
	return false
}

func (c comparator) exact(v *semver.Version) bool {
	if v.Major() != c.major {
		return false
	}
	if minor, ok := c.minor.Get(); ok && v.Minor() != minor {
		return false
	}
	if patch, ok := c.patch.Get(); ok && v.Patch() != patch {
		return false
	}
	return v.Prerelease() == c.pre
}

// ordered reports whether v is strictly above (sign 1) or below (sign -1)
// the comparator; a part the comparator leaves out decides nothing.
func (c comparator) ordered(v *semver.Version, sign int) bool {
	if v.Major() != c.major {
		return cmp.Compare(v.Major(), c.major) == sign
	}
	minor, ok := c.minor.Get()
	if !ok {
		return false
	}
	if v.Minor() != minor {
		return cmp.Compare(v.Minor(), minor) == sign
	}
	patch, ok := c.patch.Get()
	if !ok {
		return false
	}
	if v.Patch() != patch {
		return cmp.Compare(v.Patch(), patch) == sign
	}
	return comparePre(v.Prerelease(), c.pre) == sign
}

func (c comparator) tilde(v *semver.Version) bool {
	if v.Major() != c.major {
		return false
	}
	if minor, ok := c.minor.Get(); ok && v.Minor() != minor {
		return false
	}
	if patch, ok := c.patch.Get(); ok && v.Patch() != patch {
		return v.Patch() > patch
	}
	return comparePre(v.Prerelease(), c.pre) >= 0
}

func (c comparator) caret(v *semver.Version) bool {
	if v.Major() != c.major {
		return false
	}
	minor, ok := c.minor.Get()
	if !ok {
		return true
	}
	patch, ok := c.patch.Get()
	if !ok {
		if c.major > 0 {
			return v.Minor() >= minor
		}
		return v.Minor() == minor
	}
	switch {
	case c.major > 0:
		if v.Minor() != minor {
			return v.Minor() > minor
		}
		if v.Patch() != patch {
			return v.Patch() > patch
		}
	case minor > 0:
		if v.Minor() != minor {
			return false
		}
		if v.Patch() != patch {
			return v.Patch() > patch
		}
	default:
		if v.Minor() != minor || v.Patch() != patch {
			return false
		}
	}
	return comparePre(v.Prerelease(), c.pre) >= 0
}

// comparePre orders pre-release strings by SemVer precedence; the empty
// string (a release) is above every pre-release.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	return compareIdentifiers(a, b, false)
}

// compareIdentifiers orders dot-separated identifiers: digit runs by value
// and below text, text in ASCII order, the longer list last. Build metadata
// may carry leading zeros, which break ties (`1 < 01`).
func compareIdentifiers(a, b string, build bool) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i, x := range as {
		if i >= len(bs) {
			return 1
		}
		y := bs[i]
		xNum, yNum := allDigits(x), allDigits(y)
		var order int
		switch {
		case xNum && yNum && build:
			xv, yv := strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			order = cmp.Or(cmp.Compare(len(xv), len(yv)), strings.Compare(xv, yv), cmp.Compare(len(x), len(y)))
		case xNum && yNum:
			order = cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y))
		case xNum:
			return -1
		case yNum:
			return 1
		default:
			order = strings.Compare(x, y)
		}
		if order != 0 {
			return order
		}
	}
	if len(bs) > len(as) {
		return -1
	}
	return 0
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// compareVersions is the total order of the Rust crate: precedence first,
// then build metadata, so that equal releases still sort the same way.
func compareVersions(a, b *semver.Version) int {
	return cmp.Or(
		cmp.Compare(a.Major(), b.Major()),
		cmp.Compare(a.Minor(), b.Minor()),
		cmp.Compare(a.Patch(), b.Patch()),
		comparePre(a.Prerelease(), b.Prerelease()),
		compareBuild(a.Metadata(), b.Metadata()),
	)
}

func compareBuild(a, b string) int {
	if a == b {
		return 0
	}
	return compareIdentifiers(a, b, true)
}

// position is where in a version the parser stands, for its messages.
type position string

const (
	posMajor position = "major version number"
	posMinor position = "minor version number"
	posPatch position = "patch version number"
	posPre   position = "pre-release identifier"
	posBuild position = "build metadata"
)

const maxComparators = 32

// parseVersionReq reads a range. The error texts are those of the Rust
// crate: they are shown to the user inside the pattern error.
func parseVersionReq(text string) (VersionReq, error) {
	text = strings.TrimLeft(text, " ")
	if ch, rest, ok := wildcard(text); ok {
		rest = strings.TrimLeft(rest, " ")
		switch {
		case rest == "":
			return VersionReq{}, nil
		case strings.HasPrefix(rest, ","):
			return VersionReq{}, wildcardNotAlone(ch)
		default:
			return VersionReq{}, errAfterWildcard()
		}
	}
	var out []comparator
	for {
		c, pos, rest, err := parseComparator(text)
		if err != nil {
			if ch, after, ok := wildcard(text); ok {
				after = strings.TrimLeft(after, " ")
				if after == "" || strings.HasPrefix(after, ",") {
					return VersionReq{}, wildcardNotAlone(ch)
				}
			}
			return VersionReq{}, err
		}
		out = append(out, c)
		if rest == "" {
			return VersionReq{comparators: out}, nil
		}
		after, ok := strings.CutPrefix(rest, ",")
		if !ok {
			return VersionReq{}, fmt.Errorf("expected comma after %s, found %s", pos, quoteChar(rest))
		}
		if len(out) == maxComparators {
			return VersionReq{}, fmt.Errorf("excessive number of version comparators")
		}
		text = strings.TrimLeft(after, " ")
	}
}

func wildcardNotAlone(ch byte) error {
	return fmt.Errorf("wildcard req (%c) must be the only comparator in the version req", ch)
}

func errAfterWildcard() error {
	return fmt.Errorf("unexpected character after wildcard in version req")
}

func wildcard(input string) (byte, string, bool) {
	if input != "" && (input[0] == '*' || input[0] == 'x' || input[0] == 'X') {
		return input[0], input[1:], true
	}
	return 0, input, false
}

func parseOp(input string) (op, string) {
	switch {
	case strings.HasPrefix(input, "="):
		return opExact, input[1:]
	case strings.HasPrefix(input, ">="):
		return opGreaterEq, input[2:]
	case strings.HasPrefix(input, ">"):
		return opGreater, input[1:]
	case strings.HasPrefix(input, "<="):
		return opLessEq, input[2:]
	case strings.HasPrefix(input, "<"):
		return opLess, input[1:]
	case strings.HasPrefix(input, "~"):
		return opTilde, input[1:]
	case strings.HasPrefix(input, "^"):
		return opCaret, input[1:]
	}
	return opCaret, input
}

// parseComparator reads one comparator and returns what follows it, with
// the position reached for the caller's message.
func parseComparator(input string) (comparator, position, string, error) {
	o, text := parseOp(input)
	defaultOp := len(text) == len(input)
	text = strings.TrimLeft(text, " ")

	pos := posMajor
	major, text, err := numeric(text, pos)
	if err != nil {
		return comparator{}, pos, "", err
	}
	c := comparator{op: o, major: major}
	hasWildcard := false

	if rest, ok := strings.CutPrefix(text, "."); ok {
		pos = posMinor
		if _, after, isWild := wildcard(rest); isWild {
			hasWildcard = true
			text = after
		} else {
			minor, after, err := numeric(rest, pos)
			if err != nil {
				return comparator{}, pos, "", err
			}
			c.minor, text = opt.Some(minor), after
		}
	}
	if rest, ok := strings.CutPrefix(text, "."); ok {
		pos = posPatch
		if _, after, isWild := wildcard(rest); isWild {
			hasWildcard = true
			text = after
		} else if hasWildcard {
			return comparator{}, pos, "", errAfterWildcard()
		} else {
			patch, after, err := numeric(rest, pos)
			if err != nil {
				return comparator{}, pos, "", err
			}
			c.patch, text = opt.Some(patch), after
		}
	}
	if hasWildcard && defaultOp {
		c.op = opWildcard
	}
	if c.patch.IsSome() {
		if pos, text, err = parseSuffixes(&c, pos, text); err != nil {
			return comparator{}, pos, "", err
		}
	}
	return c, pos, strings.TrimLeft(text, " "), nil
}

// parseSuffixes reads `-pre` and `+build` after a full version; the build
// metadata is checked and dropped.
func parseSuffixes(c *comparator, pos position, text string) (position, string, error) {
	if rest, ok := strings.CutPrefix(text, "-"); ok {
		pos = posPre
		pre, after, err := identifier(rest, pos)
		if err != nil {
			return pos, "", err
		}
		if pre == "" {
			return pos, "", fmt.Errorf("empty identifier segment in %s", pos)
		}
		c.pre, text = pre, after
	}
	if rest, ok := strings.CutPrefix(text, "+"); ok {
		pos = posBuild
		build, after, err := identifier(rest, pos)
		if err != nil {
			return pos, "", err
		}
		if build == "" {
			return pos, "", fmt.Errorf("empty identifier segment in %s", pos)
		}
		text = after
	}
	return pos, text, nil
}

func numeric(input string, pos position) (uint64, string, error) {
	n := 0
	for n < len(input) && input[n] >= '0' && input[n] <= '9' {
		n++
	}
	switch {
	case n == 0 && input == "":
		return 0, "", fmt.Errorf("unexpected end of input while parsing %s", pos)
	case n == 0:
		return 0, "", fmt.Errorf("unexpected character %s while parsing %s", quoteChar(input), pos)
	case n > 1 && input[0] == '0':
		return 0, "", fmt.Errorf("invalid leading zero in %s", pos)
	}
	value, err := strconv.ParseUint(input[:n], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("value of %s exceeds u64::MAX", pos)
	}
	return value, input[n:], nil
}

// identifier reads dot-separated `[0-9A-Za-z-]+` segments and returns them
// with what follows; nothing at all is the empty identifier.
func identifier(input string, pos position) (string, string, error) {
	done, segment, nonDigit := 0, 0, false
	for {
		at := done + segment
		if at < len(input) && isIdentByte(input[at]) {
			nonDigit = nonDigit || input[at] < '0' || input[at] > '9'
			segment++
			continue
		}
		dot := at < len(input) && input[at] == '.'
		if segment == 0 {
			if done == 0 && !dot {
				return "", input, nil
			}
			return "", "", fmt.Errorf("empty identifier segment in %s", pos)
		}
		if pos == posPre && segment > 1 && !nonDigit && input[done] == '0' {
			return "", "", fmt.Errorf("invalid leading zero in %s", pos)
		}
		done += segment
		if !dot {
			return input[:done], input[done:], nil
		}
		done++
		segment, nonDigit = 0, false
	}
}

func isIdentByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '-'
}

// quoteChar shows the first character of s the way Rust's `{:?}` shows a
// char.
func quoteChar(s string) string {
	r, _ := utf8.DecodeRuneInString(s)
	switch r {
	case 0:
		return `'\0'`
	case '\t':
		return `'\t'`
	case '\r':
		return `'\r'`
	case '\n':
		return `'\n'`
	case '\\':
		return `'\\'`
	case '\'':
		return `'\''`
	}
	if !strconv.IsPrint(r) {
		return fmt.Sprintf(`'\u{%x}'`, r)
	}
	return "'" + string(r) + "'"
}
