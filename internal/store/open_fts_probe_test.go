package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenReportsFTSAvailableForInitializedSQLiteDB pins an invariant the
// read-only open paths already hold: a Store knows whether full-text search is
// available in the database it just opened. Until Open probes for it, a store
// opened against an already-initialized database reports FTS as unavailable
// until something calls InitSchema again, and every search it serves silently
// falls back to the slow path.
func TestOpenReportsFTSAvailableForInitializedSQLiteDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	first, err := Open(dbPath)
	require.NoError(t, err, "open store")
	require.NoError(t, first.InitSchema(), "init schema")
	require.True(t, first.FTS5Available(), "FTS is available once the schema is initialized")
	require.NoError(t, first.Close(), "close store")

	second, err := Open(dbPath)
	require.NoError(t, err, "reopen store")
	t.Cleanup(func() { _ = second.Close() })

	assert.True(t, second.FTS5Available(), "reopening an initialized database reports FTS as available")
}

// TestOpenReportsFTSAvailableForInitializedPostgresSchema is the PostgreSQL
// half of the same invariant: there the FTS column lives in the messages table,
// so a second Store opened on an initialized schema must see it too.
func TestOpenReportsFTSAvailableForInitializedPostgresSchema(t *testing.T) {
	dbURL := skipUnlessPostgresInternal(t)

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "msgvault_test_" + hex.EncodeToString(buf)

	adminDB, err := sql.Open("pgx", dbURL)
	require.NoError(t, err, "open setup connection")
	t.Cleanup(func() { _ = adminDB.Close() })
	_, err = adminDB.Exec("CREATE SCHEMA " + schemaName)
	require.NoErrorf(t, err, "create schema %s", schemaName)
	t.Cleanup(func() { _, _ = adminDB.Exec("DROP SCHEMA IF EXISTS " + schemaName + " CASCADE") })

	separator := "?"
	if strings.Contains(dbURL, "?") {
		separator = "&"
	}
	schemaURL := dbURL + separator + "search_path=" + schemaName

	first, err := Open(schemaURL)
	require.NoError(t, err, "open store")
	require.NoError(t, first.InitSchema(), "init schema")
	require.True(t, first.FTS5Available(), "FTS is available once the schema is initialized")
	require.NoError(t, first.Close(), "close store")

	second, err := Open(schemaURL)
	require.NoError(t, err, "reopen store")
	t.Cleanup(func() { _ = second.Close() })

	assert.True(t, second.FTS5Available(), "reopening an initialized schema reports FTS as available")
}
