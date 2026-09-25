// Package journal is the installer's journal
// (crates/kuben/src/cli/setup/journal.rs, M2.6, plan §11.2): every `kuben
// setup` run, the outcome of each of its steps, and who owns each resource
// the installer touched — Kuben, when setup created it, or someone else,
// when it was there first. A resource's owner is decided the first time
// setup sees it and never changes, so `kuben uninstall --purge` removes only
// what setup made (I12) and a repeated install never takes over what it did
// not create.
//
// The file lives at /var/lib/kuben/install/journal.json, written by root
// atomically after every step and readable by the `kuben` service user;
// `kuben serve` copies it into SQL once the database is up.
package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	wire "github.com/Teamtem-dev/kuben/internal/jsonx"
)

// File is the journal, relative to the state directory.
const File = "install/journal.json"

const (
	journalFormat = 1
	// maxRuns is how many runs are kept; older ones are dropped.
	maxRuns = 20
)

// StepResult is how a step ended.
type StepResult string

// The step results, with their wire strings.
const (
	// StepChanged: the step changed the machine.
	StepChanged StepResult = "changed"
	// StepUnchanged: everything was already as it should be.
	StepUnchanged StepResult = "unchanged"
	StepFailed    StepResult = "failed"
)

// ParseStepResult reads a wire string.
func ParseStepResult(s string) (StepResult, error) {
	switch r := StepResult(s); r {
	case StepChanged, StepUnchanged, StepFailed:
		return r, nil
	}
	return "", fmt.Errorf("unknown variant `%s`, expected one of `changed`, `unchanged`, `failed`", s)
}

// UnmarshalJSON refuses a string that is no step result.
func (r *StepResult) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err //nolint:wrapcheck // a JSON error
	}
	v, err := ParseStepResult(s)
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// Kind is something the installer touched.
type Kind string

// The resource kinds, with their wire strings.
const (
	KindFile             Kind = "file"
	KindDirectory        Kind = "directory"
	KindSystemUser       Kind = "systemUser"
	KindSystemdUnit      Kind = "systemdUnit"
	KindPackage          Kind = "package"
	KindPostgresRole     Kind = "postgresRole"
	KindPostgresDatabase Kind = "postgresDatabase"
	// KindCluster is the Kubernetes distribution (k3s).
	KindCluster      Kind = "cluster"
	KindFirewallRule Kind = "firewallRule"
	// KindKubernetesObject is a Kubernetes object, named
	// `kind/namespace/name` or `kind/name`.
	KindKubernetesObject Kind = "kubernetesObject"
)

// ParseKind reads a wire string.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindFile, KindDirectory, KindSystemUser, KindSystemdUnit, KindPackage, KindPostgresRole,
		KindPostgresDatabase, KindCluster, KindFirewallRule, KindKubernetesObject:
		return k, nil
	}
	return "", fmt.Errorf("unknown variant `%s`", s)
}

// UnmarshalJSON refuses a string that is no kind.
func (k *Kind) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err //nolint:wrapcheck // a JSON error
	}
	v, err := ParseKind(s)
	if err != nil {
		return err
	}
	*k = v
	return nil
}

// DebugName is the kind as Rust's `{:?}` printed it (`PostgresDatabase`),
// for `kuben setup --plan`.
func (k Kind) DebugName() string {
	if k == "" {
		return ""
	}
	return strings.ToUpper(string(k[:1])) + string(k[1:])
}

// Owner says who made a resource.
type Owner string

// The owners, with their wire strings.
const (
	// OwnerKuben: setup created it; uninstall may remove it.
	OwnerKuben Owner = "kuben"
	// OwnerPreexisting: it was there before setup first looked; never
	// removed or taken over.
	OwnerPreexisting Owner = "preexisting"
)

// UnmarshalJSON refuses a string that is no owner.
func (o *Owner) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err //nolint:wrapcheck // a JSON error
	}
	switch v := Owner(s); v {
	case OwnerKuben, OwnerPreexisting:
		*o = v
		return nil
	}
	return fmt.Errorf("unknown variant `%s`, expected `kuben` or `preexisting`", s)
}

// DebugName is the owner as Rust's `{:?}` printed it (`Kuben`).
func (o Owner) DebugName() string {
	switch o {
	case OwnerKuben:
		return "Kuben"
	case OwnerPreexisting:
		return "Preexisting"
	}
	return string(o)
}

// Journal is the file's content.
type Journal struct {
	Format    uint32     `json:"format"`
	Runs      []Run      `json:"runs"`
	Resources []Resource `json:"resources"`
}

// Run is one `kuben setup`.
type Run struct {
	// Kuben is the kuben version that ran.
	Kuben      string         `json:"kuben"`
	StartedAt  int64          `json:"startedAt"`
	FinishedAt opt.Val[int64] `json:"finishedAt,omitzero"`
	// Succeeded is absent while running, or when the run was interrupted.
	Succeeded opt.Val[bool] `json:"succeeded,omitzero"`
	Steps     []Step        `json:"steps"`
}

// Step is the outcome of one step of a run.
type Step struct {
	ID     string     `json:"id"`
	Result StepResult `json:"result"`
	Detail string     `json:"detail"`
	At     int64      `json:"at"`
}

// Resource is something setup has seen, with its owner.
type Resource struct {
	Kind  Kind   `json:"kind"`
	Name  string `json:"name"`
	Owner Owner  `json:"owner"`
	Since int64  `json:"since"`
}

// MarshalJSON writes empty lists as `[]`, as serde wrote a Vec.
func (j Journal) MarshalJSON() ([]byte, error) {
	type plain Journal
	p := plain(j)
	if p.Runs == nil {
		p.Runs = []Run{}
	}
	if p.Resources == nil {
		p.Resources = []Resource{}
	}
	return marshal(p)
}

// MarshalJSON writes an empty step list as `[]`.
func (r Run) MarshalJSON() ([]byte, error) {
	type plain Run
	p := plain(r)
	if p.Steps == nil {
		p.Steps = []Step{}
	}
	return marshal(p)
}

// marshal is json.Marshal without HTML escaping: serde_json wrote `<`, `>`
// and `&` as they are.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err //nolint:wrapcheck // plain data
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// UnmarshalJSON reads the journal as serde did: format is required, the
// lists default to empty.
func (j *Journal) UnmarshalJSON(data []byte) error {
	var o wire.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // a JSON error
	}
	var out Journal
	if err := wire.Required(o, "format", &out.Format); err != nil {
		return err //nolint:wrapcheck // names the member
	}
	if err := optionalList(o, "runs", &out.Runs); err != nil {
		return err
	}
	if err := optionalList(o, "resources", &out.Resources); err != nil {
		return err
	}
	*j = out
	return nil
}

// UnmarshalJSON reads a run: kuben and startedAt are required.
func (r *Run) UnmarshalJSON(data []byte) error {
	var o wire.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // a JSON error
	}
	var out Run
	if err := wire.Required(o, "kuben", &out.Kuben); err != nil {
		return err //nolint:wrapcheck // names the member
	}
	if err := wire.Required(o, "startedAt", &out.StartedAt); err != nil {
		return err //nolint:wrapcheck // names the member
	}
	if err := wire.Optional(o, "finishedAt", &out.FinishedAt); err != nil {
		return err //nolint:wrapcheck // names the member
	}
	if err := wire.Optional(o, "succeeded", &out.Succeeded); err != nil {
		return err //nolint:wrapcheck // names the member
	}
	if err := optionalList(o, "steps", &out.Steps); err != nil {
		return err
	}
	*r = out
	return nil
}

// UnmarshalJSON reads a step: every member is required.
func (s *Step) UnmarshalJSON(data []byte) error {
	var o wire.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // a JSON error
	}
	var out Step
	for _, err := range []error{
		wire.Required(o, "id", &out.ID),
		wire.Required(o, "result", &out.Result),
		wire.Required(o, "detail", &out.Detail),
		wire.Required(o, "at", &out.At),
	} {
		if err != nil {
			return err //nolint:wrapcheck // names the member
		}
	}
	*s = out
	return nil
}

// UnmarshalJSON reads a resource: every member is required.
func (r *Resource) UnmarshalJSON(data []byte) error {
	var o wire.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // a JSON error
	}
	var out Resource
	for _, err := range []error{
		wire.Required(o, "kind", &out.Kind),
		wire.Required(o, "name", &out.Name),
		wire.Required(o, "owner", &out.Owner),
		wire.Required(o, "since", &out.Since),
	} {
		if err != nil {
			return err //nolint:wrapcheck // names the member
		}
	}
	*r = out
	return nil
}

// optionalList decodes a `#[serde(default)]` list: absent is empty, null is
// refused as serde refused it.
func optionalList[T any](o wire.Object, key string, out *[]T) error {
	raw, ok := o[key]
	if !ok {
		*out = nil
		return nil
	}
	if wire.IsNull(raw) {
		return fmt.Errorf("field `%s`: invalid type: null, expected a sequence", key)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("field `%s`: %w", key, err)
	}
	return nil
}

// fresh is an empty journal of the current format.
func fresh() Journal { return Journal{Format: journalFormat} }

// read reads path; a missing file is an empty journal. An unreadable
// one is moved aside (it is a record, never an input to a decision that
// could destroy data) and a fresh journal starts.
func read(path string, now int64, logger *slog.Logger) (Journal, error) {
	text, err := os.ReadFile(path) //nolint:gosec // the installer's own file
	if errors.Is(err, fs.ErrNotExist) {
		return fresh(), nil
	}
	if err != nil {
		return Journal{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var j Journal
	if err := json.Unmarshal(text, &j); err != nil {
		aside := withExtension(path, fmt.Sprintf("unreadable-%d", now))
		_ = os.Rename(path, aside) //nolint:errcheck // a fresh journal starts either way
		logger.Warn("the install journal was unreadable; starting a new one", "error", err, "kept", aside)
		return fresh(), nil
	}
	return j, nil
}

// Peek reads path without touching it: false when it is missing or
// unreadable (for `--plan` and `kuben serve`).
func Peek(path string) (Journal, bool) {
	text, err := os.ReadFile(path) //nolint:gosec // the installer's own file
	if err != nil {
		return Journal{}, false
	}
	var j Journal
	if err := json.Unmarshal(text, &j); err != nil {
		return Journal{}, false
	}
	return j, true
}

// OwnerOf is who owns name, if setup has seen it.
func (j *Journal) OwnerOf(kind Kind, name string) (Owner, bool) {
	for _, r := range j.Resources {
		if r.Kind == kind && r.Name == name {
			return r.Owner, true
		}
	}
	return "", false
}

// Owns reports whether setup created name (a journal-less install says
// nothing).
func (j *Journal) Owns(kind Kind, name string) bool {
	owner, ok := j.OwnerOf(kind, name)
	return ok && owner == OwnerKuben
}

// Unfinished is the last run, when it did not finish well.
func (j *Journal) Unfinished() (Run, bool) {
	if len(j.Runs) == 0 {
		return Run{}, false
	}
	last := j.Runs[len(j.Runs)-1]
	if ok, known := last.Succeeded.Get(); known && ok {
		return Run{}, false
	}
	return last, true
}

// Claim records name as seen: created when setup made it just now. The
// first sighting decides the owner for good.
func (j *Journal) Claim(kind Kind, name string, created bool, now int64) Owner {
	if owner, ok := j.OwnerOf(kind, name); ok {
		return owner
	}
	owner := OwnerPreexisting
	if created {
		owner = OwnerKuben
	}
	j.Resources = append(j.Resources, Resource{Kind: kind, Name: name, Owner: owner, Since: now})
	return owner
}

// Release forgets name after uninstall removed it.
func (j *Journal) Release(kind Kind, name string) {
	j.Resources = slices.DeleteFunc(j.Resources, func(r Resource) bool { return r.Kind == kind && r.Name == name })
}

func (j *Journal) begin(kuben string, now int64) {
	j.Runs = append(j.Runs, Run{Kuben: kuben, StartedAt: now})
	if excess := len(j.Runs) - maxRuns; excess > 0 {
		j.Runs = slices.Delete(j.Runs, 0, excess)
	}
}

func (j *Journal) record(id string, result StepResult, detail string, now int64) {
	if len(j.Runs) == 0 {
		return
	}
	last := &j.Runs[len(j.Runs)-1]
	last.Steps = append(last.Steps, Step{ID: id, Result: result, Detail: detail, At: now})
}

func (j *Journal) finish(succeeded bool, now int64) {
	if len(j.Runs) == 0 {
		return
	}
	last := &j.Runs[len(j.Runs)-1]
	last.FinishedAt = opt.Some(now)
	last.Succeeded = opt.Some(succeeded)
}

// LastRunChanged reports whether the last run changed anything.
func (j *Journal) LastRunChanged() bool {
	if len(j.Runs) == 0 {
		return false
	}
	return slices.ContainsFunc(j.Runs[len(j.Runs)-1].Steps, func(s Step) bool { return s.Result != StepUnchanged })
}

// Book is a journal bound to its file; every change is saved at once.
type Book struct {
	path    string
	journal Journal
	// group is the service user's group, which may read the file.
	group opt.Val[uint32]
	// current is the step in progress, recorded as failed if the run stops
	// in it.
	current opt.Val[string]
	clock   clock.Clock
}

// OpenBook opens the journal under stateDir, readable by group.
func OpenBook(stateDir string, group opt.Val[uint32], c clock.Clock, logger *slog.Logger) (*Book, error) {
	path := filepath.Join(stateDir, File)
	j, err := read(path, c.NowMs(), logger)
	if err != nil {
		return nil, err
	}
	return &Book{path: path, journal: j, group: group, clock: c}, nil
}

// SetGroup lets the service user's group read the journal from now on.
func (b *Book) SetGroup(gid uint32) { b.group = opt.Some(gid) }

// Journal is the journal as it stands.
func (b *Book) Journal() *Journal { return &b.journal }

// Begin starts a run of kuben (a version).
func (b *Book) Begin(kuben string) error {
	b.journal.begin(kuben, b.clock.NowMs())
	return b.save()
}

// Start begins a step; Done or Finish closes it.
func (b *Book) Start(id string) { b.current = opt.Some(id) }

// Done ends the step in progress, having changed the machine or not.
func (b *Book) Done(changed bool, detail string) error {
	id := b.current.Or("step")
	b.current = opt.None[string]()
	result := StepUnchanged
	if changed {
		result = StepChanged
	}
	b.journal.record(id, result, detail, b.clock.NowMs())
	return b.save()
}

// Claim is Journal.Claim, saved.
func (b *Book) Claim(kind Kind, name string, created bool) (Owner, error) {
	owner := b.journal.Claim(kind, name, created, b.clock.NowMs())
	return owner, b.save()
}

// Release is Journal.Release, saved.
func (b *Book) Release(kind Kind, name string) error {
	b.journal.Release(kind, name)
	return b.save()
}

// Finish ends the run; failure names why it stopped, recorded on the step
// in progress.
func (b *Book) Finish(failure error) error {
	id, inStep := b.current.Get()
	b.current = opt.None[string]()
	if inStep && failure != nil {
		b.journal.record(id, StepFailed, failure.Error(), b.clock.NowMs())
	}
	b.journal.finish(failure == nil, b.clock.NowMs())
	return b.save()
}

// save writes then renames, so a crash leaves the old journal or the new
// one. The file is serde_json's pretty form with a final newline.
func (b *Book) save() error {
	dir := filepath.Dir(b.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	restrict(dir, 0o750, b.group)
	staged := withExtension(b.path, "json.new")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b.journal); err != nil {
		return fmt.Errorf("writing %s: %w", staged, err)
	}
	if err := writeSynced(staged, buf.Bytes()); err != nil {
		return fmt.Errorf("writing %s: %w", staged, err)
	}
	restrict(staged, 0o640, b.group)
	if err := os.Rename(staged, b.path); err != nil {
		return fmt.Errorf("writing %s: %w", b.path, err)
	}
	return nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640) //nolint:gosec // the installer's own file
	if err != nil {
		return err //nolint:wrapcheck // the caller names the file
	}
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close() //nolint:wrapcheck // the caller names the file
}

// restrict makes path owned by root's choice of group (the service user
// may read) with mode; failures are ignored, as Rust did.
func restrict(path string, mode os.FileMode, group opt.Val[uint32]) {
	_ = os.Chmod(path, mode) //nolint:errcheck // best effort, as in Rust
	if gid, ok := group.Get(); ok {
		_ = os.Chown(path, -1, int(gid)) //nolint:errcheck // best effort, as in Rust
	}
}

// withExtension is Rust's Path::with_extension: the extension of the last
// component replaced by ext.
func withExtension(path, ext string) string {
	dir, base := filepath.Split(path)
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return dir + base + "." + ext
}
