// Package pgtest is test support for the store (ADR-025): tests run
// against a real PostgreSQL, each in a schema of its own, so they run in
// parallel without sharing rows. It replaces the Rust module
// crates/kuben-store/src/testing.rs.
//
// [Store] and [Schema] read KUBEN_TEST_PG_URL. Without it the test skips
// with a message, unless KUBEN_REQUIRE_PG=1 (CI sets it), which turns the
// skip into a failure: no database test is ever skipped silently. A
// configured but unreachable server always fails the test.
package pgtest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// URLVar names the test server, e.g.
// `postgres://postgres:kuben@localhost:5432/postgres`.
const URLVar = "KUBEN_TEST_PG_URL"

// RequireVar set to 1 makes a missing server a failure instead of a skip.
const RequireVar = "KUBEN_REQUIRE_PG"

// cleanupTimeout bounds dropping a test schema.
const cleanupTimeout = 30 * time.Second

// Store is a migrated store on a fresh schema, closed and dropped when the
// test ends (up to 4 connections, as in Rust).
func Store(t testing.TB) *store.Store {
	t.Helper()
	pc := Schema(t)
	s, err := store.ConnectConfig(t.Context(), pc, 4)
	if err != nil {
		t.Fatalf("migrate the test schema: %v", err)
	}
	// Registered after Schema's cleanup, so it runs first: the pool closes
	// before the schema is dropped.
	t.Cleanup(s.Close)
	return s
}

// Schema is the pool configuration of a fresh, empty schema (its
// search_path), dropped when the test ends: for tests of the migrator
// itself.
func Schema(t testing.TB) *pgxpool.Config {
	t.Helper()
	url := os.Getenv(URLVar)
	if url == "" {
		if os.Getenv(RequireVar) == "1" {
			t.Fatalf("%s=1 but %s is not set: this database test must run", RequireVar, URLVar)
		}
		t.Skipf("skipped %s: set %s to run it against PostgreSQL", t.Name(), URLVar)
	}
	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", URLVar, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("name the test schema: %v", err)
	}
	schema := "t_" + strings.ReplaceAll(id.String(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatalf("%s is set but the server is unreachable: %v", URLVar, err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+ident); err != nil { //nolint:gosec // a sanitized identifier made above
		t.Fatalf("create the test schema: %v", err)
	}
	if err := admin.Close(t.Context()); err != nil {
		t.Fatalf("close the admin connection: %v", err)
	}
	t.Cleanup(func() { drop(t, url, ident) })
	pc.ConnConfig.RuntimeParams["search_path"] = schema
	return pc
}

// drop removes a test schema with everything in it.
func drop(t testing.TB, url, ident string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Errorf("drop the test schema: %v", err)
		return
	}
	defer func() {
		if err := admin.Close(ctx); err != nil {
			t.Errorf("close the admin connection: %v", err)
		}
	}()
	if _, err := admin.Exec(ctx, "DROP SCHEMA "+ident+" CASCADE"); err != nil { //nolint:gosec // a sanitized identifier
		t.Errorf("drop the test schema: %v", err)
	}
}
