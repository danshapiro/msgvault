package testutil

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// requirePostgresTestURL returns the configured PostgreSQL test URL, skipping
// the calling test when MSGVAULT_TEST_DB is unset or points at SQLite.
func requirePostgresTestURL(t *testing.T) string {
	t.Helper()

	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PostgreSQL-only: set MSGVAULT_TEST_DB to a postgres:// URL")
	}

	return testDB
}

// currentSchemaOf reports the schema a store's connections resolve unqualified
// names against, which is the per-test schema the fixture handed it.
func currentSchemaOf(t *testing.T, st *store.Store) string {
	t.Helper()

	var name string
	require.NoError(t, st.DB().QueryRow("SELECT current_schema()").Scan(&name), "select current_schema")

	return name
}

// schemaExists reports whether a schema of the given name is present.
func schemaExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int
	require.NoError(t,
		db.QueryRow("SELECT count(*) FROM pg_namespace WHERE nspname = $1", name).Scan(&count),
		"count pg_namespace rows")

	return count > 0
}

// countSources reports how many rows the store's sources table holds. A fresh
// fixture has none; a fixture that shares a schema with another test does not.
func countSources(t *testing.T, st *store.Store) int {
	t.Helper()

	var count int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM sources").Scan(&count), "count sources")

	return count
}

// randomSchemaSuffix returns a suffix in the same shape the fixture uses.
func randomSchemaSuffix(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema suffix")

	return hex.EncodeToString(buf)
}

// createSchemaForTest creates a schema and registers its removal, so a test can
// stage schemas for the sweeper to consider without leaking them on failure.
func createSchemaForTest(t *testing.T, db *sql.DB, name string) {
	t.Helper()

	_, err := db.Exec("CREATE SCHEMA " + name)
	require.NoErrorf(t, err, "create schema %s", name)
	t.Cleanup(func() {
		_, _ = db.Exec("DROP SCHEMA IF EXISTS " + name + " CASCADE")
	})
}

// reapedPID returns the pid of a process that has run to completion and been
// waited for, so the kernel no longer lists it under /proc.
func reapedPID(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("/bin/true")
	require.NoError(t, cmd.Run(), "run short-lived helper process")
	pid := cmd.Process.Pid

	_, statErr := os.Stat("/proc/" + strconv.Itoa(pid))
	require.True(t, os.IsNotExist(statErr), "reaped helper pid must be absent from /proc")

	return pid
}

// TestPostgresFixturesGetPrivateEmptySchemas locks the fixture contract the
// warm pool must not weaken: two fixtures in one binary get different schemas,
// each starts empty, and writes through one are invisible to the other.
func TestPostgresFixturesGetPrivateEmptySchemas(t *testing.T) {
	requirePostgresTestURL(t)

	first := NewTestStore(t)
	second := NewTestStore(t)

	firstSchema := currentSchemaOf(t, first)
	secondSchema := currentSchemaOf(t, second)
	assert.NotEqual(t, firstSchema, secondSchema, "fixtures must not share a schema")

	assert.Equal(t, 0, countSources(t, first), "first fixture starts empty")
	assert.Equal(t, 0, countSources(t, second), "second fixture starts empty")

	_, err := first.GetOrCreateSource("gmail", "isolation@example.com")
	require.NoError(t, err, "write through the first fixture")

	assert.Equal(t, 1, countSources(t, first), "write lands in the first fixture")
	assert.Equal(t, 0, countSources(t, second), "write must not leak into the second fixture")
}

// TestPostgresFixtureSchemaDroppedAfterCleanup proves the claiming test still
// owns removal of whatever schema it was handed.
func TestPostgresFixtureSchemaDroppedAfterCleanup(t *testing.T) {
	dbURL := requirePostgresTestURL(t)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(t, err, "open admin connection")

	var schema string
	t.Run("owner", func(t *testing.T) {
		schema = currentSchemaOf(t, NewTestStore(t))
	})

	require.NotEmpty(t, schema, "subtest recorded its schema")
	assert.False(t, schemaExists(t, adminDB, schema), "claimed schema is gone after the owning test's cleanup")
}

// TestSweepWarmSchemasDropsOnlyDeadOwners covers the sweeper's liveness rule:
// a warm schema whose creating process is gone is reclaimed, one whose owner is
// still running is left alone.
func TestSweepWarmSchemasDropsOnlyDeadOwners(t *testing.T) {
	dbURL := requirePostgresTestURL(t)
	if runtime.GOOS != "linux" {
		t.Skip("process liveness is only decidable via /proc on Linux")
	}

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(t, err, "open admin connection")

	dead := fmt.Sprintf("%s%d_%s", warmSchemaPrefix, reapedPID(t), randomSchemaSuffix(t))
	self := fmt.Sprintf("%s%d_%s", warmSchemaPrefix, os.Getpid(), randomSchemaSuffix(t))
	otherLive := fmt.Sprintf("%s%d_%s", warmSchemaPrefix, 1, randomSchemaSuffix(t))
	createSchemaForTest(t, adminDB, dead)
	createSchemaForTest(t, adminDB, self)
	createSchemaForTest(t, adminDB, otherLive)

	dropped, err := sweepWarmSchemas(adminDB, processAlive)
	require.NoError(t, err, "sweep warm schemas")

	assert.Contains(t, dropped, dead, "sweep reports the reclaimed schema")
	assert.False(t, schemaExists(t, adminDB, dead), "dead owner's warm schema is reclaimed")
	assert.True(t, schemaExists(t, adminDB, self), "this process's own warm schema survives")
	assert.True(t, schemaExists(t, adminDB, otherLive), "a live owner's warm schema survives")
}

// TestSweepWarmSchemasNeverDropsTestSchemas is the safety test: other agents'
// in-flight fixtures use the msgvault_test_ prefix on this shared server, and no
// liveness verdict may ever put one of those in the sweeper's sights.
func TestSweepWarmSchemasNeverDropsTestSchemas(t *testing.T) {
	dbURL := requirePostgresTestURL(t)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(t, err, "open admin connection")

	// A fixture-shaped schema, plus one that mimics the warm naming inside the
	// test prefix — neither may be touched.
	testSchema := "msgvault_test_" + randomSchemaSuffix(t)
	lookalike := fmt.Sprintf("msgvault_test_p%d_%s", 1, randomSchemaSuffix(t))
	createSchemaForTest(t, adminDB, testSchema)
	createSchemaForTest(t, adminDB, lookalike)

	// Bait: a genuine warm schema owned by a pid this sweep will call dead, so
	// the survival assertions below cannot pass vacuously.
	strangerPID := reapedPID(t)
	bait := fmt.Sprintf("%s%d_%s", warmSchemaPrefix, strangerPID, randomSchemaSuffix(t))
	createSchemaForTest(t, adminDB, bait)

	// Declare only the bait's owner dead: sibling test binaries sharing this
	// server keep their warm schemas.
	dropped, err := sweepWarmSchemas(adminDB, func(pid int) bool { return pid != strangerPID })
	require.NoError(t, err, "sweep warm schemas")

	assert.Contains(t, dropped, bait, "sweep did drop something in this run")
	for _, name := range dropped {
		assert.True(t, strings.HasPrefix(name, warmSchemaPrefix),
			"sweep may only ever drop names carrying the warm prefix, got %q", name)
	}
	assert.True(t, schemaExists(t, adminDB, testSchema), "a msgvault_test_ schema must survive the sweep")
	assert.True(t, schemaExists(t, adminDB, lookalike), "a warm-looking msgvault_test_ schema must survive the sweep")
}

// TestParseWarmSchemaName pins which names the sweeper is even able to consider.
func TestParseWarmSchemaName(t *testing.T) {
	suffix := "0123456789abcdef"

	t.Run("accepts a name this package generated", func(t *testing.T) {
		name, err := newWarmSchemaName()
		require.NoError(t, err, "generate warm schema name")

		pid, parsedSuffix, ok := parseWarmSchemaName(name)
		require.True(t, ok, "generated names must round-trip")
		assert.Equal(t, os.Getpid(), pid, "name carries the creating pid")
		assert.Equal(t, name, fmt.Sprintf("%s%d_%s", warmSchemaPrefix, pid, parsedSuffix), "parts rebuild the name")
	})

	rejected := []string{
		"msgvault_test_" + suffix,
		"msgvault_test_p1_" + suffix,
		"msgvault_warm_" + suffix,
		"msgvault_warm_pfoo_" + suffix,
		"msgvault_warm_p_" + suffix,
		"msgvault_warm_p-1_" + suffix,
		"msgvault_warm_p1_" + suffix + "; DROP SCHEMA msgvault_test_x",
		"msgvault_warm_p1_" + strings.ToUpper(suffix),
		"public",
		"",
		" msgvault_warm_p1_" + suffix,
	}
	for _, name := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			_, _, ok := parseWarmSchemaName(name)
			assert.False(t, ok, "name %q must not be sweepable", name)
		})
	}
}

// TestWarmPoolServesInitializedSchemas proves the pool's product is a real,
// already-migrated schema, not just a name.
func TestWarmPoolServesInitializedSchemas(t *testing.T) {
	dbURL := requirePostgresTestURL(t)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(t, err, "open admin connection")

	pool := warmPoolFor(dbURL)

	var name string
	require.Eventually(t, func() bool {
		select {
		case claimed, ok := <-pool.names:
			name = claimed
			return ok
		default:
			return false
		}
	}, 60*time.Second, 20*time.Millisecond, "warm pool produced a schema")
	t.Cleanup(func() {
		_, _ = adminDB.Exec("DROP SCHEMA IF EXISTS " + name + " CASCADE")
	})

	assert.True(t, strings.HasPrefix(name, warmSchemaPrefix), "warm schemas are self-owned, got %q", name)

	var messagesTable sql.NullString
	require.NoError(t, adminDB.QueryRow("SELECT to_regclass($1)", name+".messages").Scan(&messagesTable),
		"look up the messages table in the warm schema")
	assert.True(t, messagesTable.Valid, "warm schema already carries the initialized DDL")
}

// TestNewTestStoreFallsBackWhenWarmPoolDisabled covers the path a pool outage
// takes: the fixture creates its own schema and behaves exactly as before.
func TestNewTestStoreFallsBackWhenWarmPoolDisabled(t *testing.T) {
	requirePostgresTestURL(t)
	t.Setenv(warmPoolDisableEnv, "0")

	st := NewTestStore(t)

	schema := currentSchemaOf(t, st)
	assert.True(t, strings.HasPrefix(schema, "msgvault_test_"),
		"the fallback path creates its own schema, got %q", schema)
	assert.Equal(t, 0, countSources(t, st), "fallback fixture starts empty")

	_, err := st.GetOrCreateSource("gmail", "fallback@example.com")
	require.NoError(t, err, "write through the fallback fixture")
	assert.Equal(t, 1, countSources(t, st), "fallback fixture is writable")
}
