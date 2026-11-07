package whatsapp

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func testBackup(t *testing.T, rosterSQL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "msgstore.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(`
        PRAGMA journal_mode=WAL;
        CREATE TABLE jid (_id INTEGER PRIMARY KEY, user TEXT, server TEXT, raw_string TEXT);
        CREATE TABLE chat (_id INTEGER PRIMARY KEY, jid_row_id INTEGER, hidden INTEGER, subject TEXT, sort_timestamp INTEGER, group_type INTEGER);
        CREATE TABLE message (_id INTEGER PRIMARY KEY, chat_row_id INTEGER, from_me INTEGER, key_id TEXT, sender_jid_row_id INTEGER, timestamp INTEGER, message_type INTEGER, text_data TEXT, status INTEGER, starred INTEGER);
        CREATE TABLE message_media (message_row_id INTEGER, mime_type TEXT, file_size INTEGER, file_path TEXT, width INTEGER, height INTEGER, media_duration INTEGER);
        CREATE TABLE jid_map (lid_row_id INTEGER, jid_row_id INTEGER);
        INSERT INTO jid VALUES (1, '12345', 'g.us', '12345@g.us');
        INSERT INTO jid VALUES (2, '11111', 'lid', '11111@lid');
        INSERT INTO jid VALUES (3, '15550001111', 's.whatsapp.net', '15550001111@s.whatsapp.net');
        INSERT INTO jid VALUES (4, '22222', 'lid', '22222@lid');
        INSERT INTO jid VALUES (5, '33333', 'lid_me', '33333@lid_me');
        INSERT INTO jid_map VALUES (2, 3);
        INSERT INTO chat VALUES (10, 1, 0, 'Synthetic group', 1700000000000, 1);
        INSERT INTO message VALUES (1, 10, 0, 'message-1', 2, 1700000000000, 0, 'hello', 0, 0);
        INSERT INTO message VALUES (2, 10, 1, 'message-2', NULL, 1700000001000, 0, 'reply', 0, 0);
    ` + rosterSQL)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

const modernRoster = `
CREATE TABLE group_participant_user (_id INTEGER, group_jid_row_id INTEGER, user_jid_row_id INTEGER, rank INTEGER, pending INTEGER);
INSERT INTO group_participant_user VALUES (1, 1, 2, 1, 0);
INSERT INTO group_participant_user VALUES (2, 1, 5, 0, 0);
`

func TestImportModernRosterAndIdempotentReimport(t *testing.T) {
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)

	first, err := imp.Import(context.Background(), backup, opts)
	require.NoError(t, err)
	assert.Zero(t, first.Errors)
	assert.Equal(t, "complete", first.GroupRoster)
	assert.EqualValues(t, 2, first.MessagesAdded)
	assert.Zero(t, first.MessagesUpdated)
	var role string
	err = st.DB().QueryRow(`SELECT cp.role FROM conversation_participants cp JOIN participants p ON p.id=cp.participant_id JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE pi.identifier_value=?`, "+15550001111").Scan(&role)
	require.NoError(t, err)
	assert.Equal(t, "admin", role)
	// Simulate an earlier sender-only import: authoritative roster promotes it.
	_, err = st.DB().Exec(`UPDATE conversation_participants SET role='member' WHERE role='admin'`)
	require.NoError(t, err)
	second, err := imp.Import(context.Background(), backup, opts)
	require.NoError(t, err)
	assert.Zero(t, second.Errors)
	assert.Zero(t, second.MessagesAdded)
	assert.EqualValues(t, 2, second.MessagesUpdated)
	err = st.DB().QueryRow(`SELECT role FROM conversation_participants WHERE role='admin'`).Scan(&role)
	require.NoError(t, err)
	var total int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&total))
	assert.Equal(t, 2, total)
}

func TestImportMissingOrUnresolvedRosterIsFailedSync(t *testing.T) {
	for _, tc := range []struct{ name, roster, status string }{
		{"unsupported", "", "unavailable"},
		{"unresolved", modernRoster + `INSERT INTO group_participant_user VALUES (3, 1, 4, 0, 0);`, "partial"},
		{"pending", modernRoster + `INSERT INTO group_participant_user VALUES (3, 1, 4, 0, 1);`, "partial"},
		{"unknown rank", modernRoster + `INSERT INTO group_participant_user VALUES (3, 1, 4, 9, 0);`, "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backup := testBackup(t, tc.roster)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			result, err := NewImporter(st, nil).Import(context.Background(), backup, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.status, result.GroupRoster)
			assert.Positive(t, result.Errors)
			assert.EqualValues(t, 2, result.MessagesAdded)
			var status string
			var errorCount int
			require.NoError(t, st.DB().QueryRow(`SELECT status, errors_count FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &errorCount))
			assert.Equal(t, "failed", status)
			assert.Positive(t, errorCount)
		})
	}
}

func TestImportCriticalBodyWriteFailureIsReported(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite failure-injection trigger")
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_body BEFORE INSERT ON message_bodies BEGIN SELECT RAISE(FAIL, 'injected failure'); END`)
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(context.Background(), backup, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 2, result.ErrorCounts["message_body"])
	var status string
	var errorCount int
	require.NoError(t, st.DB().QueryRow(`SELECT status, errors_count FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &errorCount))
	assert.Equal(t, "failed", status)
	assert.Positive(t, errorCount)
}

// A private backup may be supplied from a secure ephemeral location for an
// operator-run integration check. The decrypted SQLite copy must first be
// normalized to WAL mode. No message contents are printed.
func TestImportSuppliedBackup(t *testing.T) {
	path := os.Getenv("MSGVAULT_WHATSAPP_TEST_BACKUP")
	if path == "" {
		t.Skip("no external backup supplied")
	}
	st := testutil.NewSQLiteTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	first, err := imp.Import(context.Background(), path, opts)
	require.NoError(t, err)
	t.Logf("first run roster=%s errors=%d categories=%v processed=%d added=%d updated=%d skipped=%d", first.GroupRoster, first.Errors, first.ErrorCounts, first.MessagesProcessed, first.MessagesAdded, first.MessagesUpdated, first.MessagesSkipped)
	require.Positive(t, first.MessagesProcessed)
	second, err := imp.Import(context.Background(), path, opts)
	require.NoError(t, err)
	t.Logf("second run roster=%s errors=%d categories=%v processed=%d added=%d updated=%d skipped=%d", second.GroupRoster, second.Errors, second.ErrorCounts, second.MessagesProcessed, second.MessagesAdded, second.MessagesUpdated, second.MessagesSkipped)
	assert.Zero(t, second.MessagesAdded)
	assert.Equal(t, first.MessagesAdded, second.MessagesUpdated)
	assert.Equal(t, first.Errors, second.Errors)
	assert.Equal(t, first.ErrorCounts, second.ErrorCounts)
}

func TestImportLegacyRoster(t *testing.T) {
	backup := testBackup(t, `CREATE TABLE group_participants(gjid TEXT, jid TEXT, admin INTEGER);
INSERT INTO group_participants VALUES ('12345@g.us', '15550001111@s.whatsapp.net', 2);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(context.Background(), backup, opts)
	require.NoError(t, err)
	assert.Zero(t, result.Errors)
	assert.Equal(t, "complete", result.GroupRoster)
	var role string
	require.NoError(t, st.DB().QueryRow(`SELECT role FROM conversation_participants WHERE role='admin'`).Scan(&role))
	assert.Equal(t, "admin", role)
}

type cancelAfterProgress struct {
	NullProgress
	cancel context.CancelFunc
}

func (p cancelAfterProgress) OnProgress(_, _, _ int64) { p.cancel() }

func TestImportCancellationMarksSyncFailed(t *testing.T) {
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	opts.BatchSize = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := NewImporter(st, cancelAfterProgress{cancel: cancel}).Import(ctx, backup, opts)
	require.ErrorIs(t, err, context.Canceled)
	var status string
	var processed int
	require.NoError(t, st.DB().QueryRow(`SELECT status, messages_processed FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &processed))
	assert.Equal(t, "failed", status)
	assert.Equal(t, 1, processed)
}

func TestUnresolvedRosterMemberCanBeResolvedOnLaterImport(t *testing.T) {
	backup := testBackup(t, modernRoster+`INSERT INTO group_participant_user VALUES (3, 1, 4, 1, 0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	first, err := imp.Import(context.Background(), backup, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, first.ErrorCounts["group_roster_unresolved"])
	assert.Equal(t, "partial", first.GroupRoster)

	// An updated source backup provides an authoritative mapping later.
	db, err := sql.Open("sqlite3", backup)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO jid VALUES (6, '15550003333', 's.whatsapp.net', '15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (4, 6);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	second, err := imp.Import(context.Background(), backup, opts)
	require.NoError(t, err)
	assert.Zero(t, second.Errors)
	assert.Equal(t, "complete", second.GroupRoster)
	assert.EqualValues(t, 2, second.MessagesUpdated)
	var role string
	err = st.DB().QueryRow(`SELECT cp.role FROM conversation_participants cp JOIN participant_identifiers pi ON pi.participant_id=cp.participant_id WHERE pi.identifier_value=?`, "+15550003333").Scan(&role)
	require.NoError(t, err)
	assert.Equal(t, "admin", role)
}

func TestImportMissingMessageKeyIsNotSuccessful(t *testing.T) {
	backup := testBackup(t, modernRoster)
	db, err := sql.Open("sqlite3", backup)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE message SET key_id='' WHERE _id=1`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(context.Background(), backup, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.SkippedCounts["missing_key"])
	assert.EqualValues(t, 1, result.ErrorCounts["message_key"])
	var status string
	require.NoError(t, st.DB().QueryRow(`SELECT status FROM sync_runs`).Scan(&status))
	assert.Equal(t, "failed", status)
}

func TestImportRejectsMissingCriticalColumnBeforeWriting(t *testing.T) {
	backup := testBackup(t, modernRoster)
	db, err := sql.Open("sqlite3", backup)
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE message DROP COLUMN timestamp`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	_, err = NewImporter(st, nil).Import(context.Background(), backup, opts)
	require.ErrorContains(t, err, "missing message.timestamp")
	var sources int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&sources))
	assert.Zero(t, sources)
}

func TestModernRosterSuperAdminIsRecordedAsAdmin(t *testing.T) {
	roster := strings.Replace(modernRoster, "(1, 1, 2, 1, 0)", "(1, 1, 2, 2, 0)", 1)
	backup := testBackup(t, roster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(context.Background(), backup, opts)
	require.NoError(t, err)
	assert.Zero(t, result.Errors)
	var role string
	require.NoError(t, st.DB().QueryRow(`SELECT cp.role FROM conversation_participants cp JOIN participant_identifiers pi ON pi.participant_id=cp.participant_id WHERE pi.identifier_value=?`, "+15550001111").Scan(&role))
	assert.Equal(t, "admin", role)
}
