package whatsapp

import (
	"context"
	"database/sql"
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
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "msgstore.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `
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
    `+rosterSQL)
	require.NoError(err)
	require.NoError(db.Close())
	return path
}

const modernRoster = `
CREATE TABLE group_participant_user (_id INTEGER, group_jid_row_id INTEGER, user_jid_row_id INTEGER, rank INTEGER, pending INTEGER);
INSERT INTO group_participant_user VALUES (1, 1, 2, 1, 0);
INSERT INTO group_participant_user VALUES (2, 1, 5, 0, 0);
`

func TestImportModernRosterAndIdempotentReimport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)

	first, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Zero(first.Errors)
	assert.Equal("complete", first.GroupRoster)
	assert.EqualValues(2, first.MessagesAdded)
	assert.Zero(first.MessagesUpdated)
	var role string
	err = st.DB().QueryRowContext(t.Context(), `SELECT cp.role FROM conversation_participants cp JOIN participants p ON p.id=cp.participant_id JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE pi.identifier_value=?`, "+15550001111").Scan(&role)
	require.NoError(err)
	assert.Equal("admin", role)
	// Simulate an earlier sender-only import: authoritative roster promotes it.
	_, err = st.DB().ExecContext(t.Context(), `UPDATE conversation_participants SET role='member' WHERE role='admin'`)
	require.NoError(err)
	second, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Zero(second.Errors)
	assert.Zero(second.MessagesAdded)
	assert.EqualValues(2, second.MessagesUpdated)
	err = st.DB().QueryRowContext(t.Context(), `SELECT role FROM conversation_participants WHERE role='admin'`).Scan(&role)
	require.NoError(err)
	var total int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&total))
	assert.Equal(2, total)
}

func TestImportMissingOrUnresolvedRosterIsFailedSync(t *testing.T) {
	for _, tc := range []struct{ name, roster, status string }{
		{"unsupported", "", "unavailable"},
		{"unresolved", modernRoster + `INSERT INTO group_participant_user VALUES (3, 1, 4, 0, 0);`, "partial"},
		{"pending", modernRoster + `INSERT INTO group_participant_user VALUES (3, 1, 4, 0, 1);`, "partial"},
		{"unknown rank", modernRoster + `INSERT INTO group_participant_user VALUES (3, 1, 4, 9, 0);`, "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backup := testBackup(t, tc.roster)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
			require.NoError(err)
			assert.Equal(tc.status, result.GroupRoster)
			assert.Positive(result.Errors)
			assert.EqualValues(2, result.MessagesAdded)
			var status string
			var errorCount int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status, errors_count FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &errorCount))
			assert.Equal("failed", status)
			assert.Positive(errorCount)
		})
	}
}

func TestImportCriticalBodyWriteFailureIsReported(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	testutil.SkipIfPostgres(t, "SQLite failure-injection trigger")
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_body BEFORE INSERT ON message_bodies BEGIN SELECT RAISE(FAIL, 'injected failure'); END`)
	require.NoError(err)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.EqualValues(2, result.ErrorCounts["message_body"])
	var status string
	var errorCount int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status, errors_count FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &errorCount))
	assert.Equal("failed", status)
	assert.Positive(errorCount)
}

func TestImportLegacyRoster(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, `CREATE TABLE group_participants(gjid TEXT, jid TEXT, admin INTEGER);
INSERT INTO group_participants VALUES ('12345@g.us', '15550001111@s.whatsapp.net', 2);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Zero(result.Errors)
	assert.Equal("complete", result.GroupRoster)
	var role string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT role FROM conversation_participants WHERE role='admin'`).Scan(&role))
	assert.Equal("admin", role)
}

type cancelAfterProgress struct {
	NullProgress

	cancel context.CancelFunc
}

func (p cancelAfterProgress) OnProgress(_, _, _ int64) { p.cancel() }

func TestImportCancellationMarksSyncFailed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	opts.BatchSize = 1
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := NewImporter(st, cancelAfterProgress{cancel: cancel}).Import(ctx, backup, opts)
	require.ErrorIs(err, context.Canceled)
	var status string
	var processed int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status, messages_processed FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &processed))
	assert.Equal("failed", status)
	assert.Equal(1, processed)
}

func TestUnresolvedRosterMemberCanBeResolvedOnLaterImport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster+`INSERT INTO group_participant_user VALUES (3, 1, 4, 1, 0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	first, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.EqualValues(1, first.ErrorCounts["group_roster_unresolved"])
	assert.Equal("partial", first.GroupRoster)

	// An updated source backup provides an authoritative mapping later.
	db, err := sql.Open("sqlite3", backup)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO jid VALUES (6, '15550003333', 's.whatsapp.net', '15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (4, 6);`)
	require.NoError(err)
	require.NoError(db.Close())
	second, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Zero(second.Errors)
	assert.Equal("complete", second.GroupRoster)
	assert.EqualValues(2, second.MessagesUpdated)
	var role string
	err = st.DB().QueryRowContext(t.Context(), `SELECT cp.role FROM conversation_participants cp JOIN participant_identifiers pi ON pi.participant_id=cp.participant_id WHERE pi.identifier_value=?`, "+15550003333").Scan(&role)
	require.NoError(err)
	assert.Equal("admin", role)
}

func TestImportMissingMessageKeyIsNotSuccessful(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster)
	db, err := sql.Open("sqlite3", backup)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `UPDATE message SET key_id='' WHERE _id=1`)
	require.NoError(err)
	require.NoError(db.Close())
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.EqualValues(1, result.SkippedCounts["missing_key"])
	assert.EqualValues(1, result.ErrorCounts["message_key"])
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs`).Scan(&status))
	assert.Equal("failed", status)
}

func TestImportRejectsMissingCriticalColumnBeforeWriting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster)
	db, err := sql.Open("sqlite3", backup)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `ALTER TABLE message DROP COLUMN timestamp`)
	require.NoError(err)
	require.NoError(db.Close())
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	_, err = NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.ErrorContains(err, "missing message.timestamp")
	var sources int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sources`).Scan(&sources))
	assert.Zero(sources)
}

func TestModernRosterSuperAdminIsRecordedAsAdmin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	roster := strings.Replace(modernRoster, "(1, 1, 2, 1, 0)", "(1, 1, 2, 2, 0)", 1)
	backup := testBackup(t, roster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Zero(result.Errors)
	var role string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT cp.role FROM conversation_participants cp JOIN participant_identifiers pi ON pi.participant_id=cp.participant_id WHERE pi.identifier_value=?`, "+15550001111").Scan(&role))
	assert.Equal("admin", role)
}

type cancelAfterChat struct {
	NullProgress

	cancel context.CancelFunc
}

func (p cancelAfterChat) OnChatComplete(string, int64) { p.cancel() }

func TestImportCancellationAfterFinalChatMarksSyncFailed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := NewImporter(st, cancelAfterChat{cancel: cancel}).Import(ctx, backup, opts)
	require.ErrorIs(err, context.Canceled)
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs`).Scan(&status))
	assert.Equal("failed", status)
}

func TestOrphanRosterReferencesAreNotSilent(t *testing.T) {
	cases := []struct{ name, roster string }{
		{"modern", modernRoster + `INSERT INTO group_participant_user VALUES (3, 999, 2, 0, 0); INSERT INTO group_participant_user VALUES (4, 1, 999, 0, 0);`},
		{"legacy", `CREATE TABLE group_participants(gjid TEXT, jid TEXT, admin INTEGER);
		INSERT INTO group_participants VALUES ('missing@g.us', '15550001111@s.whatsapp.net', 0);
		INSERT INTO group_participants VALUES ('12345@g.us', 'missing@s.whatsapp.net', 0);`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backup := testBackup(t, tc.roster)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
			require.NoError(err)
			assert.Equal("partial", result.GroupRoster)
			assert.EqualValues(1, result.ErrorCounts["group_roster_orphan_group"])
			assert.EqualValues(1, result.ErrorCounts["group_roster_unresolved"])
			assert.EqualValues(2, result.MessagesAdded)
			var status string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs`).Scan(&status))
			assert.Equal("failed", status)
		})
	}
}

func TestHiddenAndOrphanChatMessagesAreCounted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster+`
	INSERT INTO jid VALUES (7, '12346', 'g.us', '12346@g.us');
	INSERT INTO chat VALUES (20, 7, 1, 'Hidden group', 1700000002000, 1);
	INSERT INTO message VALUES (3, 20, 0, 'hidden-message', 2, 1700000002000, 0, 'not imported', 0, 0);
	INSERT INTO message VALUES (4, 999, 0, 'orphan-message', 2, 1700000003000, 0, 'not routed', 0, 0);
	INSERT INTO chat VALUES (21, 998, 0, 'Missing JID', 1700000004000, 1);
	INSERT INTO message VALUES (5, 21, 0, 'orphan-jid-message', 2, 1700000004000, 0, 'not routed', 0, 0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.EqualValues(5, result.MessagesProcessed)
	assert.EqualValues(2, result.MessagesAdded)
	assert.EqualValues(3, result.MessagesSkipped)
	assert.EqualValues(1, result.SkippedCounts["hidden_chat"])
	assert.EqualValues(2, result.SkippedCounts["missing_chat"])
	assert.EqualValues(2, result.ErrorCounts["message_orphan_chat"])
}

func TestUnresolvedReimportPreservesPreviousSenderAttributionAndSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	testutil.SkipIfPostgres(t, "direct FTS5 inspection")
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	// An independently confirmed identity need not still be resolvable in a
	// later snapshot. Preserve every attribution flag on the unresolved update.
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET is_from_me=1, source_is_from_me=1, identity_is_from_me=1 WHERE source_message_id='message-1'`)
	require.NoError(err)
	var senderBefore int64
	var fromBefore string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='message-1'`).Scan(&senderBefore))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT f.from_addr FROM messages_fts f JOIN messages m ON m.id=f.message_id WHERE m.source_message_id='message-1'`).Scan(&fromBefore))
	assert.Equal("+15550001111", fromBefore)
	// Existing index text can differ from the participant's current phone.
	// A partial reimport must preserve that exact sender search value.
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages_fts SET from_addr='previous-index-sender' WHERE message_id=(SELECT id FROM messages WHERE source_message_id='message-1')`)
	require.NoError(err)
	fromBefore = "previous-index-sender"
	db, err := sql.Open("sqlite3", backup)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `DELETE FROM jid_map WHERE lid_row_id=2; UPDATE message SET text_data='updated searchable body' WHERE _id=1`)
	require.NoError(err)
	require.NoError(db.Close())
	result, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.EqualValues(1, result.ErrorCounts["sender_unresolved"])
	var senderAfter int64
	var isFrom, sourceFrom, identityFrom bool
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id,is_from_me,source_is_from_me,identity_is_from_me FROM messages WHERE source_message_id='message-1'`).Scan(&senderAfter, &isFrom, &sourceFrom, &identityFrom))
	assert.Equal(senderBefore, senderAfter)
	assert.True(isFrom)
	assert.True(sourceFrom)
	assert.True(identityFrom)
	var fromAfter string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT f.from_addr FROM messages_fts f JOIN messages m ON m.id=f.message_id WHERE m.source_message_id='message-1'`).Scan(&fromAfter))
	assert.Equal(fromBefore, fromAfter)
	var indexedBody string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT f.body FROM messages_fts f JOIN messages m ON m.id=f.message_id WHERE m.source_message_id='message-1'`).Scan(&indexedBody))
	assert.Equal("updated searchable body", indexedBody)
}

func TestRosterCoverageDoesNotClaimHistoricalMembershipIsCurrent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	db, err := sql.Open("sqlite3", backup)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `DELETE FROM group_participant_user WHERE user_jid_row_id=2`)
	require.NoError(err)
	require.NoError(db.Close())
	result, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Equal("complete", result.GroupRoster, "coverage describes source read, not stored current membership")
	var role string
	err = st.DB().QueryRowContext(t.Context(), `SELECT cp.role FROM conversation_participants cp JOIN participant_identifiers pi ON pi.participant_id=cp.participant_id WHERE pi.identifier_value=?`, "+15550001111").Scan(&role)
	require.NoError(err)
	assert.Equal("admin", role, "historical role is retained pending source-aware reconciliation")
}

func TestUnknownSenderJIDIsNotInventedAsPhone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster+`
	INSERT INTO jid VALUES (6, '123456789', 'unknown', '123456789@unknown');
	INSERT INTO message VALUES (3, 10, 0, 'unknown-sender', 6, 1700000002000, 0, 'text', 0, 0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.EqualValues(1, result.ErrorCounts["sender_unresolved"])
	var sender sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='unknown-sender'`).Scan(&sender))
	assert.False(sender.Valid)
	if st.FTS5Available() && !st.IsPostgreSQL() {
		var indexedFrom string
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT f.from_addr FROM messages_fts f JOIN messages m ON m.id=f.message_id WHERE m.source_message_id='unknown-sender'`).Scan(&indexedFrom))
		assert.Empty(indexedFrom)
	}
}

func TestDirectLIDChatResolvesSenderThroughJIDMap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster+`
	INSERT INTO chat VALUES (20, 2, 0, NULL, 1700000002000, 0);
	INSERT INTO message VALUES (3, 20, 0, 'direct-lid-message', NULL, 1700000002000, 0, 'direct', 0, 0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Zero(result.Errors)
	var phone string
	err = st.DB().QueryRowContext(t.Context(), `SELECT p.phone_number FROM messages m JOIN participants p ON p.id=m.sender_id WHERE m.source_message_id='direct-lid-message'`).Scan(&phone)
	require.NoError(err)
	assert.Equal("+15550001111", phone)
}

func TestMissingGroupSenderAndPrivacyEventAreAccountedFor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster+`
	INSERT INTO message VALUES (3, 10, 0, 'missing-sender', NULL, 1700000002000, 0, 'content', 0, 0);
	INSERT INTO message VALUES (4, 10, 0, 'dangling-sender', 999, 1700000003000, 0, 'content', 0, 0);
	INSERT INTO message VALUES (5, 10, 0, 'privacy-event', NULL, 1700000004000, 112, NULL, 0, 0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.EqualValues(2, result.ErrorCounts["sender_unresolved"])
	assert.EqualValues(1, result.SkippedCounts["type_112"])
	assert.EqualValues(5, result.MessagesProcessed)
	assert.EqualValues(4, result.MessagesAdded)
	assert.EqualValues(1, result.MessagesSkipped)
}

func TestRosterGroupWithoutChatIsPartialButHiddenChatIsRecognized(t *testing.T) {
	cases := []struct{ name, roster string }{
		{"modern", modernRoster + `INSERT INTO group_participant_user VALUES (3, 7, 2, 0, 0); INSERT INTO group_participant_user VALUES (4, 8, 2, 0, 0);`},
		{"legacy", `CREATE TABLE group_participants(gjid TEXT, jid TEXT, admin INTEGER);
		INSERT INTO group_participants VALUES ('12347@g.us', '15550001111@s.whatsapp.net', 0);
		INSERT INTO group_participants VALUES ('12348@g.us', '15550001111@s.whatsapp.net', 0);`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backup := testBackup(t, tc.roster+`
			INSERT INTO jid VALUES (7, '12347', 'g.us', '12347@g.us');
			INSERT INTO jid VALUES (8, '12348', 'g.us', '12348@g.us');
			INSERT INTO chat VALUES (20, 8, 1, 'Hidden', 1700000002000, 1);`)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
			require.NoError(err)
			assert.Equal("partial", result.GroupRoster)
			assert.EqualValues(1, result.ErrorCounts["group_roster_orphan_group"])
		})
	}
}

func TestObservedRosterRolesReconcileWithoutRemovingHistoricalMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, modernRoster)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	first, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Zero(first.Errors)

	var conversationID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM conversations WHERE source_id=?`, first.SourceID).Scan(&conversationID))
	historicalID, err := st.EnsureParticipantByPhone(t.Context(), "+15550004444", "", "whatsapp")
	require.NoError(err)
	require.NoError(st.EnsureConversationParticipantContext(t.Context(), conversationID, historicalID, "admin"))
	roleFor := func(phone string) string {
		t.Helper()
		var role string
		err := st.DB().QueryRowContext(t.Context(), `SELECT cp.role FROM conversation_participants cp JOIN participant_identifiers pi ON pi.participant_id=cp.participant_id WHERE cp.conversation_id=? AND pi.identifier_value=?`, conversationID, phone).Scan(&role)
		require.NoError(err)
		return role
	}
	assert.Equal("admin", roleFor("+15550001111"))
	assert.Equal("admin", roleFor("+15550004444"))

	// A known member's current role is authoritative even if a different
	// roster entry cannot be resolved. Historical members remain untouched.
	db, err := sql.Open("sqlite3", backup)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `UPDATE group_participant_user SET rank=0 WHERE user_jid_row_id=2;
		INSERT INTO group_participant_user VALUES (3, 1, 4, 0, 0);`)
	require.NoError(err)
	require.NoError(db.Close())
	partial, err := imp.Import(t.Context(), backup, opts)
	require.NoError(err)
	assert.Equal("partial", partial.GroupRoster)
	assert.EqualValues(1, partial.ErrorCounts["group_roster_unresolved"])
	assert.Equal("member", roleFor("+15550001111"))
	assert.Equal("admin", roleFor("+15550004444"))

	// Promotion and a repeated import are equally safe and idempotent.
	db, err = sql.Open("sqlite3", backup)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `UPDATE group_participant_user SET rank=1 WHERE user_jid_row_id=2`)
	require.NoError(err)
	require.NoError(db.Close())
	for range 2 {
		result, err := imp.Import(t.Context(), backup, opts)
		require.NoError(err)
		assert.Equal("partial", result.GroupRoster)
		assert.Equal("admin", roleFor("+15550001111"))
		assert.Equal("admin", roleFor("+15550004444"))
	}
}

func TestConflictingRosterRolesPreserveExistingRole(t *testing.T) {
	for _, existingRole := range []string{"member", "admin"} {
		t.Run(existingRole, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backup := testBackup(t, modernRoster)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			imp := NewImporter(st, nil)
			_, err := imp.Import(t.Context(), backup, opts)
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), `UPDATE conversation_participants SET role=? WHERE participant_id IN (SELECT participant_id FROM participant_identifiers WHERE identifier_value=?)`, existingRole, "+15550001111")
			require.NoError(err)
			db, err := sql.Open("sqlite3", backup)
			require.NoError(err)
			// A second LID now maps to the same phone but claims member rank,
			// conflicting with the existing admin-ranked LID in this group.
			_, err = db.ExecContext(t.Context(), `INSERT INTO jid_map VALUES (4, 3); INSERT INTO group_participant_user VALUES (3, 1, 4, 0, 0)`)
			require.NoError(err)
			require.NoError(db.Close())
			for range 2 {
				result, err := imp.Import(t.Context(), backup, opts)
				require.NoError(err)
				assert.Equal("partial", result.GroupRoster)
				assert.EqualValues(1, result.ErrorCounts["group_roster_conflicting_role"])
				var role string
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT cp.role FROM conversation_participants cp JOIN participant_identifiers pi ON pi.participant_id=cp.participant_id WHERE pi.identifier_value=?`, "+15550001111").Scan(&role))
				assert.Equal(existingRole, role)
			}
		})
	}
}
