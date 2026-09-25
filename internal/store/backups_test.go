package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

func TestWalArchivingNeedsAModeAndALevel(t *testing.T) {
	facts := func(level, mode string) store.DatabaseFacts {
		return store.DatabaseFacts{VersionNum: 170_004, WALLevel: level, ArchiveMode: mode}
	}
	if facts("replica", "on").Major() != 17 || !facts("replica", "on").ArchivesWAL() || !facts("logical", "always").ArchivesWAL() ||
		facts("replica", "off").ArchivesWAL() || facts("minimal", "on").ArchivesWAL() {
		t.Fatal("archiving needs archive_mode on/always and a WAL level above minimal")
	}
}

func TestBackupsAreRecordedAndRestoresFenceThePast(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	if v, err := s.SchemaVersion(ctx); err != nil || v < 25 {
		t.Fatalf("schema: %d %v", v, err)
	}
	facts, err := s.DatabaseFacts(ctx)
	if err != nil || facts.Major() < 15 || facts.InRecovery {
		t.Fatalf("facts: %+v %v", facts, err)
	}
	if _, ok, err := s.LastBackup(ctx); err != nil || ok {
		t.Fatalf("no backup yet: %v %v", ok, err)
	}
	record := func(succeeded bool, at int64) store.BackupRecord {
		r := store.BackupRecord{
			Scheduled: true, Succeeded: succeeded, Location: fmt.Sprintf("/backups/%d", at),
			Kuben: "2.0.0", Schema: 25, StartedAt: at - 5, FinishedAt: at,
		}
		if succeeded {
			r.Bytes, r.SHA256 = opt.Some[uint64](1024), opt.Some(strings.Repeat("a", 64))
		} else {
			r.Detail = opt.Some("pg_dump failed")
		}
		return r
	}
	for _, r := range []store.BackupRecord{record(true, 100), record(false, 200)} {
		if err := s.RecordBackup(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	last, ok, err := s.LastBackup(ctx)
	if err != nil || !ok || last.Location != "/backups/100" || last.Bytes != opt.Some[uint64](1024) || last.FinishedAt != 100 {
		t.Fatalf("last: %+v %v %v", last, ok, err)
	}

	o := org(t, s, "a", "A")
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "prod", "Prod", false))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, "a-shop"))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	commit(t, tn)

	restored, err := s.AfterRestore(ctx, 50, "1.9.0", "2.0.0")
	if err != nil || restored.TargetsRaised != 1 {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	state, ok, err := tenant(t, s, o).TargetState(ctx, tgt)
	if err != nil || !ok || uint64(state.DesiredGeneration) != uint64(store.RestoreGenerationJump) {
		t.Fatalf("the generation jumped: %+v %v %v", state, ok, err)
	}
}
