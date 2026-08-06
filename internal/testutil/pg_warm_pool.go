package testutil

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver for test setup
	"go.kenn.io/msgvault/internal/store"
)

// The PostgreSQL fixture's cost is almost entirely CREATE SCHEMA plus the
// InitSchema() DDL replay, and that work is round-trip bound rather than CPU
// bound. This file moves it off the critical path: a few background workers
// keep a small buffer of schemas that have already been created and migrated,
// and a test claims one instead of building its own. Every fixture still gets a
// private, never-used schema built by the same InitSchema() code path, and
// still drops it in t.Cleanup — nothing is shared, reused, or truncated.
const (
	// warmSchemaPrefix marks a schema as pool-owned and records the pid that
	// created it. It deliberately does not overlap the msgvault_test_ prefix
	// used for directly created fixtures: the orphan sweep only ever acts on
	// names carrying this prefix, so a fixture schema — including one belonging
	// to an unrelated process sharing the server — can never be a candidate.
	warmSchemaPrefix = "msgvault_warm_p"

	// warmPoolWorkers and warmPoolCapacity are host-safety limits, not tuning
	// knobs, and must never be derived from GOMAXPROCS or the -p flag. A shared
	// test server offers ~97 usable connections; each worker holds one while it
	// replays DDL, and the shared admin handle adds two more. Four workers keeps
	// a test binary at roughly six connections so several binaries — and several
	// people — can share one server.
	warmPoolWorkers  = 4
	warmPoolCapacity = 4

	// warmPoolDisableEnv set to "0" turns the pool off and sends every fixture
	// down the direct-creation path. An escape hatch for diagnosing whether the
	// pool is implicated in a failure.
	warmPoolDisableEnv = "MSGVAULT_TEST_PG_WARM_POOL"

	// warmWorkerMaxFailures stops a worker after this many consecutive failures
	// so a server outage does not spin. Fixtures fall back to direct creation
	// and fail with the error they would have reported without the pool.
	warmWorkerMaxFailures = 3

	// warmSchemaSuffixBytes is the entropy in a schema's random suffix.
	warmSchemaSuffixBytes = 8
)

// warmSchemaNamePattern is the sole gate on what the sweep may consider. It is
// built from warmSchemaPrefix so the prefix has one definition.
var warmSchemaNamePattern = regexp.MustCompile(
	"^" + regexp.QuoteMeta(warmSchemaPrefix) +
		"([0-9]{1,10})_([0-9a-f]{" + strconv.Itoa(warmSchemaSuffixBytes*2) + "})$")

var (
	adminDBMu  sync.Mutex
	adminDBs   = map[string]*sql.DB{}
	warmPoolMu sync.Mutex
	warmPools  = map[string]*warmSchemaPool{}
)

// pgAdminDB returns the process-wide administrative handle for a database URL,
// opening it on first use. Fixtures previously paid a TCP and authentication
// handshake to create a schema and another to drop it; sql.DB is already a
// pool, so one handle per URL serves every fixture in the binary. It is capped
// small and intentionally never closed: it lives as long as the test binary.
func pgAdminDB(dbURL string) (*sql.DB, error) {
	adminDBMu.Lock()
	defer adminDBMu.Unlock()

	if db, ok := adminDBs[dbURL]; ok {
		return db, nil
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	adminDBs[dbURL] = db

	return db, nil
}

// warmSchemaPool hands out names of schemas that have already been created and
// migrated. It serves names rather than open stores, so the buffer holds no
// database connections at rest; the claiming test opens its own handle.
type warmSchemaPool struct {
	dbURL string
	names chan string
}

// warmPoolFor returns the pool for a database URL, starting it on first use.
// Nothing runs until a PostgreSQL fixture is requested, so SQLite runs and
// packages that never touch PostgreSQL pay nothing.
func warmPoolFor(dbURL string) *warmSchemaPool {
	warmPoolMu.Lock()
	defer warmPoolMu.Unlock()

	if pool, ok := warmPools[dbURL]; ok {
		return pool
	}

	pool := &warmSchemaPool{dbURL: dbURL, names: make(chan string, warmPoolCapacity)}
	warmPools[dbURL] = pool
	pool.start()

	return pool
}

// start launches the background workers and an orphan sweep. The sweep runs in
// its own goroutine so it stays off the fixture's critical path.
func (p *warmSchemaPool) start() {
	go func() {
		db, err := pgAdminDB(p.dbURL)
		if err != nil {
			return
		}
		_, _ = sweepWarmSchemas(db, processAlive)
	}()

	var running sync.WaitGroup
	for range warmPoolWorkers {
		running.Go(p.work)
	}

	go func() {
		running.Wait()
		close(p.names)
	}()
}

// work keeps the buffer full, blocking on the send once it is. It gives up
// after repeated failures so an unreachable server does not spin; fixtures then
// fall back to direct creation and surface the real error themselves.
func (p *warmSchemaPool) work() {
	failures := 0
	for {
		name, err := p.warmOne()
		if err != nil {
			failures++
			if failures >= warmWorkerMaxFailures {
				return
			}

			continue
		}
		failures = 0
		p.names <- name
	}
}

// warmOne creates a schema, replays the production DDL into it, and closes the
// connection it used, leaving a ready-to-claim schema and no open connection.
func (p *warmSchemaPool) warmOne() (string, error) {
	db, err := pgAdminDB(p.dbURL)
	if err != nil {
		return "", err
	}

	name, err := newWarmSchemaName()
	if err != nil {
		return "", err
	}

	if _, err := db.Exec("CREATE SCHEMA " + name); err != nil {
		return "", err
	}

	st, err := store.Open(schemaURL(p.dbURL, name))
	if err != nil {
		dropSchema(db, name)

		return "", err
	}

	if err := st.InitSchema(); err != nil {
		_ = st.Close()
		dropSchema(db, name)

		return "", err
	}

	if err := st.Close(); err != nil {
		dropSchema(db, name)

		return "", err
	}

	return name, nil
}

// claimWarmSchema takes a ready schema if one is waiting. It never blocks: a
// fixture that would have to wait creates its own schema instead, so the pool
// can only make a run faster.
func claimWarmSchema(dbURL string) (string, bool) {
	if os.Getenv(warmPoolDisableEnv) == "0" {
		return "", false
	}

	select {
	case name, ok := <-warmPoolFor(dbURL).names:
		return name, ok
	default:
		return "", false
	}
}

// newWarmSchemaName returns a fresh warm schema name owned by this process.
func newWarmSchemaName() (string, error) {
	buf := make([]byte, warmSchemaSuffixBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random schema name: %w", err)
	}

	return fmt.Sprintf("%s%d_%s", warmSchemaPrefix, os.Getpid(), hex.EncodeToString(buf)), nil
}

// parseWarmSchemaName splits a warm schema name into the pid that created it
// and its random suffix. Anything that is not exactly a name this package
// generates — including every msgvault_test_ fixture schema — is rejected.
func parseWarmSchemaName(name string) (pid int, suffix string, ok bool) {
	match := warmSchemaNamePattern.FindStringSubmatch(name)
	if match == nil {
		return 0, "", false
	}

	pid, err := strconv.Atoi(match[1])
	if err != nil || pid <= 0 {
		return 0, "", false
	}

	return pid, match[2], true
}

// sweepWarmSchemas reclaims warm schemas whose creating process has exited —
// the buffer a test binary leaves behind when it stops. It returns the names it
// dropped.
//
// Two properties matter more than completeness. First, the statement it
// executes is rebuilt from the parsed pid and suffix, so its target always
// begins with warmSchemaPrefix; a msgvault_test_ schema cannot be named by this
// function no matter what the server returns. Second, liveness fails safe
// toward "alive": an unreadable /proc, an unsupported platform, or a name it
// cannot parse leaves the schema alone. A missed orphan costs a schema until
// the next run sweeps it; a wrong verdict would delete a running test's data.
func sweepWarmSchemas(db *sql.DB, alive func(pid int) bool) ([]string, error) {
	candidates, err := listWarmSchemas(db)
	if err != nil {
		return nil, err
	}

	var dropped []string
	self := os.Getpid()
	for _, candidate := range candidates {
		pid, suffix, ok := parseWarmSchemaName(candidate)
		if !ok || pid == self || alive(pid) {
			continue
		}

		// Rebuilt from validated parts, never interpolated from the row.
		target := fmt.Sprintf("%s%d_%s", warmSchemaPrefix, pid, suffix)
		if target != candidate {
			continue
		}

		if _, err := db.Exec("DROP SCHEMA " + target + " CASCADE"); err != nil {
			continue
		}
		dropped = append(dropped, target)
	}

	return dropped, nil
}

// listWarmSchemas returns the schema names carrying the warm prefix. The LIKE
// pattern is derived from the prefix constant with its underscores escaped, so
// the server is never asked about any other family of schema.
func listWarmSchemas(db *sql.DB) ([]string, error) {
	pattern := strings.ReplaceAll(warmSchemaPrefix, "_", `\_`) + "%"
	rows, err := db.Query("SELECT nspname FROM pg_namespace WHERE nspname LIKE $1", pattern)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return names, nil
}

// processAlive reports whether a pid is still running, answering "yes" whenever
// it cannot tell. Only Linux is decidable here, via /proc.
func processAlive(pid int) bool {
	if pid <= 0 || runtime.GOOS != "linux" {
		return true
	}

	if _, err := os.Stat("/proc/self"); err != nil {
		return true
	}

	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err == nil {
		return true
	}

	return !errors.Is(err, fs.ErrNotExist)
}

// dropSchema removes a schema created by this package, ignoring failure: the
// sweep reclaims anything left behind.
func dropSchema(db *sql.DB, name string) {
	_, _ = db.Exec("DROP SCHEMA IF EXISTS " + name + " CASCADE")
}

// schemaURL returns dbURL with its search_path pointed at a schema.
func schemaURL(dbURL, schemaName string) string {
	separator := "?"
	if strings.Contains(dbURL, "?") {
		separator = "&"
	}

	return dbURL + separator + "search_path=" + schemaName
}
