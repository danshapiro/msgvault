package whatsapp

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

const unresolvedIdentityFixture = modernRoster + `
INSERT INTO group_participant_user VALUES (3, 1, 4, 1, 0);
UPDATE message SET sender_jid_row_id=4 WHERE _id=1;
CREATE TABLE message_add_on (_id INTEGER PRIMARY KEY, parent_message_row_id INTEGER, sender_jid_row_id INTEGER);
CREATE TABLE message_add_on_reaction (message_add_on_row_id INTEGER, reaction TEXT, sender_timestamp INTEGER);
INSERT INTO message_add_on VALUES (10, 1, 4);
INSERT INTO message_add_on_reaction VALUES (10, '👍', 1700000005000);
`

func updateIdentityBackup(t *testing.T, path, statements string) {
	t.Helper()
	requirements := require.New(t)
	db, err := sql.Open("sqlite3", path)
	requirements.NoError(err)
	_, err = db.ExecContext(t.Context(), statements)
	requirements.NoError(err)
	requirements.NoError(db.Close())
}

func TestOpaqueIdentitiesPreserveAndResolveRosterSenderAndReaction(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)

	first, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.EqualValues(3, first.Errors)
	var originalSender sql.NullInt64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='message-1'`).Scan(&originalSender))
	requirements.True(originalSender.Valid, "an unresolved source identity must still be retained")
	var phone sql.NullString
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT phone_number FROM participants WHERE id=?`), originalSender.Int64).Scan(&phone))
	assertions.False(phone.Valid, "an internal identifier is not a phone number")
	var role string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT role FROM conversation_participants WHERE participant_id=?`), originalSender.Int64).Scan(&role))
	assertions.Equal("admin", role)
	var reactor int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT participant_id FROM reactions`).Scan(&reactor))
	assertions.Equal(originalSender.Int64, reactor)

	// Database row numbers are not durable identity across backup generations.
	updateIdentityBackup(t, backup, `UPDATE jid SET _id=44 WHERE _id=4;
UPDATE group_participant_user SET user_jid_row_id=44 WHERE user_jid_row_id=4;
UPDATE message SET sender_jid_row_id=44 WHERE sender_jid_row_id=4;
UPDATE message_add_on SET sender_jid_row_id=44 WHERE sender_jid_row_id=4;
UPDATE message_add_on SET _id=20 WHERE _id=10;
UPDATE message_add_on_reaction SET message_add_on_row_id=20 WHERE message_add_on_row_id=10;`)
	second, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.EqualValues(3, second.Errors)
	assertions.Zero(second.MessagesAdded)
	var sender int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='message-1'`).Scan(&sender))
	assertions.Equal(originalSender.Int64, sender)
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Equal(1, count)

	// The phone may already exist independently, including the same reaction.
	known, err := st.EnsureParticipantByPhone(t.Context(), "+15550003333", "", "whatsapp")
	requirements.NoError(err)
	var messageID, conversationID int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id,conversation_id FROM messages WHERE source_message_id='message-1'`).Scan(&messageID, &conversationID))
	requirements.NoError(st.SetConversationParticipantRoleContext(t.Context(), conversationID, known, "admin"))
	requirements.NoError(st.UpsertReaction(t.Context(), messageID, known, "emoji", "👍", time.Unix(1700000005, 0)))
	updateIdentityBackup(t, backup, `INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (44,6);`)
	third, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(third.Errors)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='message-1'`).Scan(&sender))
	assertions.Equal(known, sender)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT participant_id FROM reactions`).Scan(&reactor))
	assertions.Equal(known, reactor)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Equal(1, count, "resolution must merge rather than duplicate an existing reaction")
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM participants WHERE id=?`), originalSender.Int64).Scan(&count))
	assertions.Zero(count)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Equal(1, count)
}

func TestOpaqueIdentitiesAreScopedToTheSourceAccount(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	for _, account := range []string{"+15550002222", "+15550004444"} {
		opts := DefaultOptions()
		opts.Phone = account
		result, err := imp.Import(t.Context(), backup, opts)
		requirements.NoError(err)
		assertions.EqualValues(3, result.Errors)
	}
	var first, second sql.NullInt64
	rows, err := st.DB().QueryContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='message-1' ORDER BY source_id`)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	requirements.True(rows.Next())
	requirements.NoError(rows.Scan(&first))
	requirements.True(rows.Next())
	requirements.NoError(rows.Scan(&second))
	requirements.NoError(rows.Err())
	assertions.True(first.Valid)
	assertions.True(second.Valid)
	assertions.NotEqual(first.Int64, second.Int64)
}

func TestOrphanExcludedSystemEventDoesNotClaimLostSupportedMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, modernRoster+`
INSERT INTO message VALUES (3,999,1,'orphan-system',NULL,1700000002000,7,'system event',0,0);
INSERT INTO message VALUES (4,999,0,'orphan-empty',NULL,1700000003000,NULL,NULL,0,0);
INSERT INTO message VALUES (5,999,0,'orphan-text',NULL,1700000004000,0,'unroutable text',0,0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.EqualValues(5, result.MessagesProcessed)
	assertions.EqualValues(3, result.MessagesSkipped)
	assertions.EqualValues(1, result.SkippedCounts["type_7"])
	assertions.EqualValues(2, result.SkippedCounts["missing_chat"])
	assertions.EqualValues(2, result.ErrorCounts["message_orphan_chat"], "unknown empty records remain explicitly unresolved")
}

func TestOpaqueResolutionDoesNotDiscardConflictingMembershipRoles(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	var opaque, conversation int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id,conversation_id FROM messages WHERE source_message_id='message-1'`).Scan(&opaque, &conversation))
	known, err := st.EnsureParticipantByPhone(t.Context(), "+15550003333", "", "whatsapp")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationParticipantRoleContext(t.Context(), conversation, known, "member"))
	updateIdentityBackup(t, backup, `INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (4,6);`)
	result, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Positive(result.Errors)
	var sender int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='message-1'`).Scan(&sender))
	assertions.Equal(opaque, sender)
	var oldRole, newRole string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT role FROM conversation_participants WHERE participant_id=? AND conversation_id=?`), opaque, conversation).Scan(&oldRole))
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT role FROM conversation_participants WHERE participant_id=? AND conversation_id=?`), known, conversation).Scan(&newRole))
	assertions.Equal("admin", oldRole)
	assertions.Equal("member", newRole)
	var reactor int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT participant_id FROM reactions`).Scan(&reactor))
	assertions.Equal(opaque, reactor)
}

func TestConfirmedLIDCannotBeSilentlyAssignedToAnotherPhone(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture+`
INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net');
INSERT INTO jid VALUES (7,'15550004444','s.whatsapp.net','15550004444@s.whatsapp.net');
INSERT INTO jid_map VALUES (4,6);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	first, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(first.Errors)
	var before int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='message-1'`).Scan(&before))
	updateIdentityBackup(t, backup, `UPDATE jid_map SET jid_row_id=7 WHERE lid_row_id=4;`)
	result, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Positive(result.Errors)
	var after, reactor int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='message-1'`).Scan(&after))
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT participant_id FROM reactions`).Scan(&reactor))
	assertions.Equal(before, after)
	assertions.Equal(before, reactor)
}

func TestConflictingMappingsInOneBackupAreRejected(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture+`
INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net');
INSERT INTO jid VALUES (7,'15550004444','s.whatsapp.net','15550004444@s.whatsapp.net');
INSERT INTO jid_map VALUES (4,6),(4,7);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	_, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	requirements.ErrorContains(err, "conflicting authoritative phone mappings")
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&count))
	assertions.Zero(count)
}

func TestDirectOpaqueLIDWithoutSenderRowIsRetainedAndResolved(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, modernRoster+`
INSERT INTO chat VALUES (20,4,0,NULL,1700000002000,0);
INSERT INTO message VALUES (3,20,0,'direct-lid-in',NULL,1700000002000,0,'direct inbound',0,0);
INSERT INTO message VALUES (4,20,1,'direct-lid-out',NULL,1700000003000,0,'direct outbound',0,0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	first, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Positive(first.Errors)
	var opaque sql.NullInt64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='direct-lid-in'`).Scan(&opaque))
	requirements.True(opaque.Valid)
	updateIdentityBackup(t, backup, `INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (4,6);`)
	second, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(second.Errors)
	var phone string
	var messageID int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT m.id,p.phone_number FROM messages m JOIN participants p ON p.id=m.sender_id WHERE m.source_message_id='direct-lid-in'`).Scan(&messageID, &phone))
	assertions.Equal("+15550003333", phone)
	if st.FTS5Available() {
		from, err := st.PreviousFTSSenderContext(t.Context(), messageID)
		requirements.NoError(err)
		assertions.Equal(phone, from)
	}
}

func TestResolvingLIDRepairsHistoricalSenderSearchOutsideNewBackup(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	var messageID int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='message-1'`).Scan(&messageID))
	updateIdentityBackup(t, backup, `DELETE FROM message WHERE _id=1;
INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (4,6);`)
	second, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(second.Errors)
	var body string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageID).Scan(&body))
	assertions.Equal("hello", body)
	if st.FTS5Available() {
		from, err := st.PreviousFTSSenderContext(t.Context(), messageID)
		requirements.NoError(err)
		assertions.Equal("+15550003333", from)
	}
}

func TestOpaqueResolutionReconcilesChangedAndRemovedReactions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	updateIdentityBackup(t, backup, `INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (4,6);
UPDATE message_add_on_reaction SET reaction='❤️',sender_timestamp=1700000006000;`)
	result, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(result.Errors)
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Equal(1, count)
	var value string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reaction_value FROM reactions`).Scan(&value))
	assertions.Equal("❤️", value)
	updateIdentityBackup(t, backup, `UPDATE message_add_on_reaction SET reaction='',sender_timestamp=1700000007000;`)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Zero(count)
}

func TestMissingReactionSchemaPreservesPreviouslyArchivedReactions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	updateIdentityBackup(t, backup, `DROP TABLE message_add_on_reaction; DROP TABLE message_add_on;`)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Equal(1, count)
}

func TestUnreadableReactionSchemaPreservesPreviouslyArchivedReactions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	updateIdentityBackup(t, backup, `ALTER TABLE message_add_on_reaction DROP COLUMN sender_timestamp;`)
	result, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Positive(result.ErrorCounts["reaction"])
	var value string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reaction_value FROM reactions`).Scan(&value))
	assertions.Equal("👍", value)
}

func TestLatestReactionWinsButConflictingLatestPreservesArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	updateIdentityBackup(t, backup, `INSERT INTO message_add_on VALUES (11,1,4);
INSERT INTO message_add_on_reaction VALUES (11,'❤️',1700000006000);`)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	var value string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reaction_value FROM reactions`).Scan(&value))
	assertions.Equal("❤️", value)
	updateIdentityBackup(t, backup, `INSERT INTO message_add_on VALUES (12,1,4);
INSERT INTO message_add_on_reaction VALUES (12,'👀',1700000006000);`)
	result, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.EqualValues(1, result.ErrorCounts["reaction_conflict"])
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reaction_value FROM reactions`).Scan(&value))
	assertions.Equal("❤️", value, "equal latest timestamps cannot authorize replacing existing evidence")
	updateIdentityBackup(t, backup, `INSERT INTO message_add_on VALUES (13,1,4);
INSERT INTO message_add_on_reaction VALUES (13,'',1700000007000);`)
	result, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(result.ErrorCounts["reaction_conflict"])
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Zero(count, "a later removal supersedes all older observations")
}

func TestOlderBackupsCannotRevertOrResurrectReactions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	imp := NewImporter(st, nil)
	_, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	updateIdentityBackup(t, backup, `UPDATE message_add_on_reaction SET reaction='❤️',sender_timestamp=1700000006000;`)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	updateIdentityBackup(t, backup, `INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (4,6);
UPDATE message_add_on_reaction SET reaction='👍',sender_timestamp=1700000005000;`)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	var value string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reaction_value FROM reactions`).Scan(&value))
	assertions.Equal("❤️", value)
	updateIdentityBackup(t, backup, `DELETE FROM message_add_on_reaction;`)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Equal(1, count, "absence in an older backup is not a removal event")
	updateIdentityBackup(t, backup, `INSERT INTO message_add_on_reaction VALUES (10,'',1700000007000);`)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	updateIdentityBackup(t, backup, `UPDATE message_add_on_reaction SET reaction='👍',sender_timestamp=1700000005000;`)
	_, err = imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Zero(count, "explicit removal evidence must survive an older reimport")
}

func TestResolutionReconcilesHistoricalReactionsAbsentFromNewBackup(t *testing.T) {
	for _, value := range []string{"", "❤️"} {
		t.Run("latest_"+value, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			backup := testBackup(t, unresolvedIdentityFixture)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			imp := NewImporter(st, nil)
			_, err := imp.Import(t.Context(), backup, opts)
			requirements.NoError(err)
			updateIdentityBackup(t, backup, `UPDATE message_add_on_reaction SET reaction='`+value+`',sender_timestamp=1700000007000;`)
			_, err = imp.Import(t.Context(), backup, opts)
			requirements.NoError(err)
			known, err := st.EnsureParticipantByPhone(t.Context(), "+15550003333", "", "whatsapp")
			requirements.NoError(err)
			var messageID int64
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='message-1'`).Scan(&messageID))
			requirements.NoError(st.UpsertReaction(t.Context(), messageID, known, "emoji", "👍", time.UnixMilli(1700000005000)))
			updateIdentityBackup(t, backup, `DELETE FROM message WHERE _id=1;
INSERT INTO jid VALUES (6,'15550003333','s.whatsapp.net','15550003333@s.whatsapp.net'); INSERT INTO jid_map VALUES (4,6);`)
			result, err := imp.Import(t.Context(), backup, opts)
			requirements.NoError(err)
			assertions.Zero(result.Errors)
			var count int
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
			if value == "" {
				assertions.Zero(count, "a newer removal survives an identity merge without replaying the historical message")
			} else {
				assertions.Equal(1, count)
				var got string
				requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reaction_value FROM reactions`).Scan(&got))
				assertions.Equal(value, got)
			}
		})
	}
}

func TestSelfReactionRepresentationsAndDanglingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fromMe string
		sender string
		self   bool
	}{
		{"explicit_self", "1", "999", true},
		{"self_sentinel", "0", "-1", true},
		{"legacy_null", "NULL", "NULL", true},
		{"dangling_positive", "0", "999", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			backup := testBackup(t, unresolvedIdentityFixture+`ALTER TABLE message_add_on ADD COLUMN from_me INTEGER;
UPDATE message_add_on SET from_me=`+tc.fromMe+`,sender_jid_row_id=`+tc.sender+`;`)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
			requirements.NoError(err)
			var count int
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
			if tc.self {
				assertions.Equal(1, count)
				var phone string
				requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT p.phone_number FROM reactions r JOIN participants p ON p.id=r.participant_id`).Scan(&phone))
				assertions.Equal(opts.Phone, phone)
			} else {
				assertions.Zero(count)
				assertions.Positive(result.ErrorCounts["reaction"])
			}
		})
	}
}
