// Package pgtest gives each test a Postgres database of its own. `go test ./...` runs packages at
// the same time, so tests sharing the database DATABASE_URL names would see, wipe and deadlock on
// each other's rows; a database per test cannot. Only tests import it.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/postgres"
)

var made atomic.Int64

// Empty makes a new, empty database for the test on the server DATABASE_URL points at, and drops
// it when the test ends. The test is skipped when DATABASE_URL is not set or the server cannot be
// reached: that says nothing about whether the code is right.
func Empty(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping postgres integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Skipf("postgres not reachable at DATABASE_URL: %v", err)
	}
	// The process id and a counter keep names apart between packages running at once and between
	// tests in one; the time keeps them apart from anything a killed run left behind.
	name := fmt.Sprintf("kael_test_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), made.Add(1))
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create a database for the test (the DATABASE_URL user needs CREATEDB): %v", err)
	}
	cfg := admin.Config()
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		admin.Close()
	})
	return pool
}

// Store is Empty with the schema in place (postgres.Migrate).
func Store(t testing.TB) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	pool := Empty(t)
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return postgres.New(pool), pool
}
