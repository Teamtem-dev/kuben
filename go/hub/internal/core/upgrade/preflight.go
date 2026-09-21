package upgrade

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

// Level is how serious a finding is.
type Level string

// The levels, from harmless to blocking.
const (
	LevelOk   Level = "ok"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

// ParseLevel reads a level name.
func ParseLevel(s string) (Level, error) {
	switch l := Level(s); l {
	case LevelOk, LevelWarn, LevelFail:
		return l, nil
	}
	return "", kerr.New(kerr.Validation, "unknown finding level `%s`", s)
}

// Rank orders levels by seriousness: ok < warn < fail. An unknown level
// ranks below ok.
func (l Level) Rank() int {
	switch l {
	case LevelOk:
		return 0
	case LevelWarn:
		return 1
	case LevelFail:
		return 2
	}
	return -1
}

// Finding is one preflight finding.
type Finding struct {
	Level Level
	// Check names what was looked at: version, schema, backup, operations,
	// agents or disk.
	Check  string
	Detail string
}

// ProtocolRange is an inclusive range of agent protocol versions. A range
// whose Min is above its Max contains nothing.
type ProtocolRange struct {
	Min, Max uint32
}

// Contains reports whether p is in the range.
func (r ProtocolRange) Contains(p uint32) bool { return r.Min <= p && p <= r.Max }

// Agent is a linked, unrevoked agent as the preflight sees it.
type Agent struct {
	Cluster string
	// Protocol is absent until the agent linked for the first time.
	Protocol opt.Val[uint32]
}

// Facts is what the preflight looks at.
type Facts struct {
	// Running is the newest version that ran against this database, if any.
	Running opt.Val[string]
	// Target is the version about to run.
	Target string
	// Major: the operator consents to a major step.
	Major bool
	// Schema is the newest migration applied.
	Schema int64
	// TargetSchema is the newest migration the target carries.
	TargetSchema int64
	// DirtySchema: a migration failed half-way.
	DirtySchema bool
	// LastBackup is when the newest good backup finished (unix milliseconds).
	LastBackup opt.Val[int64]
	// Now is the time of the check (unix milliseconds).
	Now int64
	// ActiveOperations counts operations not settled yet.
	ActiveOperations uint64
	// Agents is every linked, unrevoked agent.
	Agents []Agent
	// TargetProtocols is the protocol versions the target speaks.
	TargetProtocols ProtocolRange
	// FreeBytes is the free space where backups and state go.
	FreeBytes opt.Val[uint64]
}

const hourMs = 3_600_000

// BackupFreshMs: a backup older than this does not protect an upgrade.
const BackupFreshMs int64 = 24 * hourMs

// MinFreeBytes: less free space than this is a warning.
const MinFreeBytes uint64 = 1 << 30

// Preflight is every finding about facts, most serious first.
func Preflight(facts Facts) []Finding {
	out := []Finding{
		versionFinding(facts),
		schemaFinding(facts),
		backupFinding(facts),
		operationsFinding(facts.ActiveOperations),
		agentsFinding(facts),
		diskFinding(facts.FreeBytes),
	}
	slices.SortStableFunc(out, func(a, b Finding) int { return cmp.Compare(b.Level.Rank(), a.Level.Rank()) })
	return out
}

func versionFinding(facts Facts) Finding {
	running, ok := facts.Running.Get()
	if !ok {
		return Finding{LevelOk, "version", "first start of " + facts.Target}
	}
	kind, err := StepBetween(running, facts.Target, facts.Major)
	switch {
	case err != nil:
		return Finding{LevelFail, "version", err.Error()}
	case kind == Same:
		return Finding{LevelOk, "version", running + " again"}
	default:
		return Finding{LevelOk, "version", fmt.Sprintf("%s → %s (%s)", running, facts.Target, kind)}
	}
}

func schemaFinding(facts Facts) Finding {
	switch {
	case facts.DirtySchema:
		return Finding{
			LevelFail, "schema",
			"a migration failed half-way: restore the pre-upgrade backup, or fix the database and mark it done",
		}
	case facts.Schema > facts.TargetSchema:
		return Finding{LevelFail, "schema", fmt.Sprintf(
			"the database is at schema %d, newer than %s knows (%d): run a newer Kuben",
			facts.Schema, facts.Target, facts.TargetSchema)}
	default:
		return Finding{LevelOk, "schema", fmt.Sprintf("%d → %d (%d migration(s))",
			facts.Schema, facts.TargetSchema, clock.SaturatingSub(facts.TargetSchema, facts.Schema))}
	}
}

func backupFinding(facts Facts) Finding {
	if at, ok := facts.LastBackup.Get(); ok {
		if age := clock.SaturatingSub(facts.Now, at); age <= BackupFreshMs {
			return Finding{LevelOk, "backup", fmt.Sprintf("the newest good backup is %d h old", age/hourMs)}
		}
	}
	if facts.TargetSchema <= facts.Schema {
		return Finding{LevelWarn, "backup", "no backup from the last 24 h (no migrations pending)"}
	}
	return Finding{
		LevelFail, "backup",
		"migrations are pending and there is no backup from the last 24 h: run `kuben backup`",
	}
}

func operationsFinding(active uint64) Finding {
	if active == 0 {
		return Finding{LevelOk, "operations", "none in flight"}
	}
	return Finding{LevelWarn, "operations", fmt.Sprintf(
		"%d in flight: they resume after the upgrade, but a quiet moment is safer", active)}
}

func agentsFinding(facts Facts) Finding {
	var stale []string
	for _, a := range facts.Agents {
		if p, ok := a.Protocol.Get(); ok && !facts.TargetProtocols.Contains(p) {
			stale = append(stale, fmt.Sprintf("%s (protocol %d)", a.Cluster, p))
		}
	}
	if len(stale) == 0 {
		return Finding{LevelOk, "agents", fmt.Sprintf("%d linked, all compatible", len(facts.Agents))}
	}
	return Finding{LevelFail, "agents", fmt.Sprintf(
		"%s would be refused by %s (it speaks %d–%d): upgrade them first",
		strings.Join(stale, ", "), facts.Target, facts.TargetProtocols.Min, facts.TargetProtocols.Max)}
}

func diskFinding(freeBytes opt.Val[uint64]) Finding {
	free, ok := freeBytes.Get()
	switch {
	case !ok:
		return Finding{LevelWarn, "disk", "free space unknown"}
	case free < MinFreeBytes:
		return Finding{LevelWarn, "disk", fmt.Sprintf("only %d MiB free for backups and state", free>>20)}
	default:
		return Finding{LevelOk, "disk", fmt.Sprintf("%d GiB free", free>>30)}
	}
}
