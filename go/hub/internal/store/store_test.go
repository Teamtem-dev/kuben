package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// Ported from db.rs.

func TestASuperuserIsReportedAsBypassingRowSecurity(t *testing.T) {
	s := pgtest.Store(t)
	// The test server connects as a superuser, as a quick local setup does.
	if _, bypasses, err := s.RoleBypassingRowSecurity(t.Context()); err != nil || !bypasses {
		t.Fatalf("role bypassing row security: %v, %v", bypasses, err)
	}
}

func TestOnlyPostgresURLsAreAccepted(t *testing.T) {
	cfg := func(url string) config.DatabaseCfg {
		return config.DatabaseCfg{URL: config.Secret(url), MaxConnections: 1}
	}
	_, err := store.Connect(t.Context(), cfg(" "))
	if !errors.Is(err, store.NotConfiguredError{}) {
		t.Errorf("blank url: %v", err)
	}
	_, err = store.Connect(t.Context(), cfg("sqlite:///data/kuben.db"))
	if !errors.Is(err, store.SQLiteError{}) {
		t.Errorf("sqlite url: %v", err)
	}
	_, err = store.Connect(t.Context(), cfg("mysql://db/kuben"))
	var unsupported store.UnsupportedURLError
	if !errors.As(err, &unsupported) || unsupported.URL != "mysql://db/kuben" {
		t.Errorf("mysql url: %v", err)
	}
}

// The messages are the Rust StoreError's.
func TestErrorMessagesAreRusts(t *testing.T) {
	for _, tc := range []struct {
		err  store.Error
		want string
	}{
		{store.UnsupportedURLError{URL: "mysql://db"}, "unsupported database url mysql://db: Kuben keeps its data in PostgreSQL (postgres://…)"},
		{store.SchemaAheadError{Database: 35, Binary: 34}, "the database is at schema 35, newer than this Kuben knows (34): it was upgraded by a newer Kuben; run that version or newer, or restore the backup taken before the upgrade"},
		{store.DirtySchemaError{}, "a database migration failed half-way (_sqlx_migrations has an unsuccessful row): restore the backup taken before the upgrade"},
		{store.MigrationError{Err: errors.New("x")}, "migration error: x"},
		{store.DatabaseError{Err: errors.New("x")}, "database error: x"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
		if k := store.Kerr(tc.err); k.Code != kerr.Internal || k.Detail != tc.want {
			t.Errorf("kerr of %q: %+v", tc.want, k)
		}
	}
	if store.IsUniqueViolation(errors.New("x")) {
		t.Error("a plain error is not a unique violation")
	}
}

// Ids travel as text (identity tables) and as uuid (product tables); pgx
// must read and write both through the ids package's Scanner and Valuer.
func TestIDsEncodeAndScanForBothColumnTypes(t *testing.T) {
	m := pgtype.NewMap()
	id := ids.New[ids.Project]()
	for _, oid := range []uint32{pgtype.TextOID, pgtype.UUIDOID} {
		for _, format := range []int16{pgtype.TextFormatCode, pgtype.BinaryFormatCode} {
			buf, err := m.Encode(oid, format, id, nil)
			if err != nil {
				t.Fatalf("encode oid %d format %d: %v", oid, format, err)
			}
			var back ids.ProjectID
			if err := m.Scan(oid, format, buf, &back); err != nil {
				t.Fatalf("scan oid %d format %d: %v", oid, format, err)
			}
			if back != id {
				t.Fatalf("oid %d format %d: %v, want %v", oid, format, back, id)
			}
			var nullable *ids.ProjectID
			if err := m.Scan(oid, format, buf, &nullable); err != nil || nullable == nil || *nullable != id {
				t.Fatalf("nullable scan oid %d format %d: %v %v", oid, format, nullable, err)
			}
			if err := m.Scan(oid, format, nil, &nullable); err != nil || nullable != nil {
				t.Fatalf("NULL scan oid %d format %d: %v %v", oid, format, nullable, err)
			}
		}
	}
	var bad ids.UserID
	if err := m.Scan(pgtype.TextOID, pgtype.TextFormatCode, []byte("not-a-uuid"), &bad); err == nil {
		t.Fatal("a text that is not a UUID must not scan into an id")
	}
	u := uuid.Must(uuid.NewV7())
	buf, err := m.Encode(pgtype.UUIDOID, pgtype.BinaryFormatCode, u, nil)
	if err != nil {
		t.Fatalf("encode uuid.UUID: %v", err)
	}
	var back uuid.UUID
	if err := m.Scan(pgtype.UUIDOID, pgtype.BinaryFormatCode, buf, &back); err != nil || back != u {
		t.Fatalf("scan uuid.UUID: %v %v", back, err)
	}
}
