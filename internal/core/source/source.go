// Package source has Git sources (M3, ADR-028): validated repository,
// branch, commit and build-recipe values, and the parts of a GitHub webhook
// Kuben acts on. It replaces the Rust module kuben-core/src/source.rs.
//
// A webhook is a hint, never the source of truth: the worker reads the
// branch head from the provider before it records a new source epoch, so a
// duplicate, reordered or forged-but-signed delivery cannot move a target
// backwards (plan §14.1).
package source

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// InvalidKind says which source value was rejected.
type InvalidKind string

// The kinds.
const (
	InvalidCommit     InvalidKind = "commit"
	InvalidRepository InvalidKind = "repository"
	InvalidBranch     InvalidKind = "branch"
	InvalidPath       InvalidKind = "path"
	// InvalidPayload: a malformed webhook payload, or an unknown name in one.
	InvalidPayload InvalidKind = "payload"
)

// Invalid is a rejected source value. Value is the rejected text; for
// InvalidPayload it is what is wrong with the payload.
type Invalid struct {
	Kind  InvalidKind
	Value string
}

func (e *Invalid) Error() string {
	switch e.Kind {
	case InvalidCommit:
		return "not a full 40-character commit SHA: " + rustQuote(e.Value)
	case InvalidRepository:
		return "not an `owner/name` repository: " + rustQuote(e.Value)
	case InvalidBranch:
		return "not a valid branch name: " + rustQuote(e.Value)
	case InvalidPath:
		return "not a relative path inside the repository: " + rustQuote(e.Value)
	case InvalidPayload:
		return "the webhook payload is malformed: " + e.Value
	}
	return "not a valid source value: " + rustQuote(e.Value)
}

// Unwrap makes the error a validation failure for errors.Is and kerrors.CodeOf.
func (e *Invalid) Unwrap() error {
	return kerrors.New(kerrors.Validation, "%s", e.Error())
}

// CommitSha is a full, lowercase commit SHA-1. Abbreviated SHAs and ref
// names are refused: a build always names exactly one tree. The zero value
// is not a commit; see [CommitSha.Valid].
type CommitSha struct{ s string }

// ParseCommitSha reads 40 lowercase hexadecimal digits.
func ParseCommitSha(s string) (CommitSha, error) {
	if len(s) != 40 {
		return CommitSha{}, &Invalid{Kind: InvalidCommit, Value: s}
	}
	for i := range len(s) {
		if !isDigit(s[i]) && (s[i] < 'a' || s[i] > 'f') {
			return CommitSha{}, &Invalid{Kind: InvalidCommit, Value: s}
		}
	}
	return CommitSha{s: s}, nil
}

// Valid reports whether the value came from [ParseCommitSha].
func (c CommitSha) Valid() bool { return c.s != "" }

func (c CommitSha) String() string { return c.s }

// Short is the first twelve digits, for names and labels; empty for the
// zero value.
func (c CommitSha) Short() string {
	if len(c.s) < 12 {
		return ""
	}
	return c.s[:12]
}

// IsZero reports the all-zero SHA GitHub sends for a created or deleted
// branch.
func (c CommitSha) IsZero() bool {
	return c.s != "" && strings.Trim(c.s, "0") == ""
}

// MarshalJSON writes the SHA as a string.
func (c CommitSha) MarshalJSON() ([]byte, error) { return json.Marshal(c.s) }

// UnmarshalJSON reads and validates a string.
func (c *CommitSha) UnmarshalJSON(data []byte) error { return unmarshalVia(data, ParseCommitSha, c) }

// RepoName is `owner/name` of a hosted repository, compared
// case-insensitively by the provider and stored lowercase here. The zero
// value is not a repository; see [RepoName.Valid].
type RepoName struct{ s string }

func repoPartOK(part string) bool {
	if part == "" || len(part) > 100 || part == "." || part == ".." {
		return false
	}
	for i := range len(part) {
		b := part[i]
		if !isAlnum(b) && b != '-' && b != '_' && b != '.' {
			return false
		}
	}
	return true
}

// ParseRepoName reads `owner/name` and lowers it.
func ParseRepoName(s string) (RepoName, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || !repoPartOK(owner) || !repoPartOK(name) {
		return RepoName{}, &Invalid{Kind: InvalidRepository, Value: s}
	}
	return RepoName{s: strings.ToLower(s)}, nil
}

// Valid reports whether the value came from [ParseRepoName].
func (r RepoName) Valid() bool { return r.s != "" }

func (r RepoName) String() string { return r.s }

// Owner is the part before the slash.
func (r RepoName) Owner() string {
	owner, _, _ := strings.Cut(r.s, "/")
	return owner
}

// Name is the part after the slash.
func (r RepoName) Name() string {
	_, name, _ := strings.Cut(r.s, "/")
	return name
}

// MarshalJSON writes `owner/name` as a string.
func (r RepoName) MarshalJSON() ([]byte, error) { return json.Marshal(r.s) }

// UnmarshalJSON reads and validates a string.
func (r *RepoName) UnmarshalJSON(data []byte) error { return unmarshalVia(data, ParseRepoName, r) }

// BranchName is a branch name under `refs/heads/`, following git's
// ref-format rules. The zero value is not a branch; see [BranchName.Valid].
type BranchName struct{ s string }

// ParseBranchName checks s against git's ref-format rules.
func ParseBranchName(s string) (BranchName, error) {
	badChar := !utf8.ValidString(s) || strings.ContainsFunc(s, func(c rune) bool {
		return unicode.IsControl(c) || unicode.IsSpace(c) || strings.ContainsRune("~^:?*[\\", c)
	})
	ok := s != "" &&
		len(s) <= 255 &&
		!badChar &&
		!strings.ContainsRune("-/.", rune(s[0])) &&
		!strings.ContainsRune("/.", rune(s[len(s)-1])) &&
		!strings.HasSuffix(s, ".lock") &&
		!strings.Contains(s, "..") &&
		!strings.Contains(s, "//") &&
		!strings.Contains(s, "@{") &&
		!strings.Contains(s, "/.")
	if !ok {
		return BranchName{}, &Invalid{Kind: InvalidBranch, Value: s}
	}
	return BranchName{s: s}, nil
}

// BranchFromRef is the branch of a full ref, if it is one.
func BranchFromRef(gitRef string) (BranchName, bool) {
	name, ok := strings.CutPrefix(gitRef, "refs/heads/")
	if !ok {
		return BranchName{}, false
	}
	b, err := ParseBranchName(name)
	return b, err == nil
}

// Valid reports whether the value came from [ParseBranchName].
func (b BranchName) Valid() bool { return b.s != "" }

func (b BranchName) String() string { return b.s }

// MarshalJSON writes the branch as a string.
func (b BranchName) MarshalJSON() ([]byte, error) { return json.Marshal(b.s) }

// UnmarshalJSON reads and validates a string.
func (b *BranchName) UnmarshalJSON(data []byte) error { return unmarshalVia(data, ParseBranchName, b) }

// RepoPath is a normalized path inside the repository: relative,
// `/`-separated, no `.` or `..` segments. The zero value is the repository
// root.
type RepoPath struct{ s string }

// ParseRepoPath normalizes s: leading `./` and trailing `/` go, and a path
// that could leave the checkout is refused.
func ParseRepoPath(s string) (RepoPath, error) {
	trimmed := s
	for strings.HasPrefix(trimmed, "./") {
		trimmed = trimmed[2:]
	}
	trimmed = strings.TrimRight(trimmed, "/")
	if trimmed == "" || trimmed == "." {
		return RepoPath{}, nil
	}
	ok := len(trimmed) <= 512 && utf8.ValidString(trimmed) && !strings.HasPrefix(trimmed, "/")
	for part := range strings.SplitSeq(trimmed, "/") {
		ok = ok && part != "" && part != "." && part != ".." &&
			!strings.ContainsFunc(part, func(c rune) bool { return unicode.IsControl(c) || c == '\\' })
	}
	if !ok {
		return RepoPath{}, &Invalid{Kind: InvalidPath, Value: s}
	}
	return RepoPath{s: trimmed}, nil
}

func (p RepoPath) String() string { return p.s }

// IsRoot reports the repository root, the empty path.
func (p RepoPath) IsRoot() bool { return p.s == "" }

// Join is `p/child`, or `child` at the root.
func (p RepoPath) Join(child RepoPath) RepoPath {
	switch {
	case p.IsRoot():
		return child
	case child.IsRoot():
		return p
	default:
		return RepoPath{s: p.s + "/" + child.s}
	}
}

// MarshalJSON writes the path as a string.
func (p RepoPath) MarshalJSON() ([]byte, error) { return json.Marshal(p.s) }

// UnmarshalJSON reads and validates a string.
func (p *RepoPath) UnmarshalJSON(data []byte) error { return unmarshalVia(data, ParseRepoPath, p) }

// unmarshalVia decodes a JSON string (and nothing else, null included) and
// stores what parse makes of it.
func unmarshalVia[T any](data []byte, parse func(string) (T, error), into *T) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return kerrors.New(kerrors.Validation, "invalid type: null, expected a string")
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := parse(s)
	if err != nil {
		return err
	}
	*into = v
	return nil
}

// BuildStrategy is how an image is built from the source. The zero value
// reads as [Auto].
type BuildStrategy string

// The strategies.
const (
	// Auto: the build pod uses the Dockerfile if there is one, Railpack
	// otherwise. Detection runs inside the build pod, never on the Kuben
	// server.
	Auto       BuildStrategy = "auto"
	Dockerfile BuildStrategy = "dockerfile"
	Railpack   BuildStrategy = "railpack"
)

// ParseBuildStrategy reads a strategy name.
func ParseBuildStrategy(s string) (BuildStrategy, error) {
	switch b := BuildStrategy(s); b {
	case Auto, Dockerfile, Railpack:
		return b, nil
	}
	return "", &Invalid{Kind: InvalidPayload, Value: "unknown build strategy " + rustQuote(s)}
}

// MarshalJSON writes the strategy name, `auto` for the zero value.
func (b BuildStrategy) MarshalJSON() ([]byte, error) { return json.Marshal(string(b.OrAuto())) }

// UnmarshalJSON reads and validates a strategy name.
func (b *BuildStrategy) UnmarshalJSON(data []byte) error {
	return unmarshalVia(data, ParseBuildStrategy, b)
}

// OrAuto is the strategy, with the zero value read as Auto.
func (b BuildStrategy) OrAuto() BuildStrategy {
	if b == "" {
		return Auto
	}
	return b
}

// BuildRecipe is the editable build plan of a source binding. Any change
// raises the target's build configuration revision, so builds of the old
// plan no longer deploy automatically.
type BuildRecipe struct {
	Strategy BuildStrategy
	// Context is the build context inside the repository (monorepo
	// sub-directory).
	Context RepoPath
	// Dockerfile is relative to Context; `Dockerfile` when unset.
	Dockerfile opt.Val[RepoPath]
}

type recipeJSON struct {
	Strategy   BuildStrategy     `json:"strategy"`
	Context    RepoPath          `json:"context"`
	Dockerfile opt.Val[RepoPath] `json:"dockerfile,omitzero"`
}

// MarshalJSON writes `strategy`, `context` and, when set, `dockerfile`.
func (r BuildRecipe) MarshalJSON() ([]byte, error) {
	return json.Marshal(recipeJSON(r))
}

// UnmarshalJSON reads a recipe; every field may be left out, and an unknown
// strategy or an escaping path is refused.
func (r *BuildRecipe) UnmarshalJSON(data []byte) error {
	raw := recipeJSON{Strategy: Auto}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = BuildRecipe(raw)
	return nil
}

// DockerfilePath is the Dockerfile path relative to the context.
func (r BuildRecipe) DockerfilePath() RepoPath {
	return r.Dockerfile.Or(RepoPath{s: "Dockerfile"})
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isAlnum(b byte) bool {
	return isDigit(b) || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// rustQuote shows s the way Rust's `{:?}` shows a string, so the messages
// stay what they were.
func rustQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == 0:
			b.WriteString(`\0`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case unicode.IsPrint(r) && !unicode.In(r, unicode.Mn, unicode.Me):
			b.WriteRune(r)
		default:
			b.WriteString(`\u{` + strconv.FormatInt(int64(r), 16) + `}`)
		}
	}
	b.WriteByte('"')
	return b.String()
}
