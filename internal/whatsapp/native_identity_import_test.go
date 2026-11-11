package whatsapp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeLIDImportCompletesWithoutInventingPhone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	importer := NewImporter(st, nil)
	for range 2 {
		result, err := importer.Import(t.Context(), backup, opts)
		require.NoError(err)
		assert.Equal(map[string]int64{"group_roster_phone_unavailable": 1, "sender_phone_unavailable": 1, "reaction_phone_unavailable": 1}, result.IdentityLimitationCounts)
		assert.EqualValues(1, result.IdentitiesWithoutPhone)
		assert.Zero(result.Errors, "retained native identity does not require a phone association")
		assert.Equal("complete", result.GroupRoster)
		var status string
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
		assert.Equal("completed", status)
	}
	var populatedPhone int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages m JOIN participants p ON p.id=m.sender_id WHERE m.source_message_id='message-1' AND COALESCE(p.phone_number,'')<>''`).Scan(&populatedPhone))
	assert.Zero(populatedPhone)
}

func TestNativeLIDBrokenPhoneMappingStillFails(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"missing phone target", `INSERT INTO jid_map VALUES(4,999);`},
		{"missing source identity", `INSERT INTO jid_map VALUES(999,3);`},
		{"empty mapped phone", `INSERT INTO jid VALUES(6,'','s.whatsapp.net',''); INSERT INTO jid_map VALUES(4,6);`},
		{"nonphone target", `INSERT INTO jid_map VALUES(4,5);`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backup := testBackup(t, unresolvedIdentityFixture+tc.change)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			_, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
			require.Error(err, "explicit broken mapping is not an optional phone limitation")
			var status string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
			assert.Equal("failed", status)
		})
	}
}

const sentinelFixture = modernRoster + `
UPDATE message SET _id=11 WHERE _id=1;
ALTER TABLE message ADD COLUMN sort_id INTEGER;
INSERT INTO message(_id,chat_row_id,from_me,key_id,sort_id) VALUES(1,-1,0,'-1',0);
`

func TestReservedMessageBaselineIsNarrowlyExcluded(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		excluded     bool
	}{
		{"exact", ``, true},
		{"ordinary empty orphan key", `UPDATE message SET key_id='unknown' WHERE _id=1`, false},
		{"dated", `UPDATE message SET timestamp=1700000000000 WHERE _id=1`, false},
		{"known type", `UPDATE message SET message_type=0 WHERE _id=1`, false},
		{"text", `UPDATE message SET text_data='retained text' WHERE _id=1`, false},
		{"sender", `UPDATE message SET sender_jid_row_id=4 WHERE _id=1`, false},
		{"self", `UPDATE message SET from_me=1 WHERE _id=1`, false},
		{"sort", `UPDATE message SET sort_id=1 WHERE _id=1`, false},
		{"different row", `UPDATE message SET _id=8 WHERE _id=1`, false},
		{"media", `INSERT INTO message_media(message_row_id,mime_type) VALUES(1,'image/jpeg')`, false},
		{"quote", `CREATE TABLE message_quoted(message_row_id INTEGER); INSERT INTO message_quoted VALUES(1)`, false},
		{"addon", `CREATE TABLE message_add_on(parent_message_row_id INTEGER); INSERT INTO message_add_on VALUES(1)`, false},
		{"future payload", `CREATE TABLE future_payload(message_row_id INTEGER); INSERT INTO future_payload VALUES(1)`, false},
		{"new nonempty column", `ALTER TABLE message ADD COLUMN future_text TEXT; UPDATE message SET future_text='data' WHERE _id=1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backup := testBackup(t, sentinelFixture+tc.change)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
			require.NoError(err)
			assert.EqualValues(3, result.MessagesProcessed)
			if tc.excluded {
				assert.Zero(result.Errors)
				assert.EqualValues(1, result.SkippedCounts["reserved_placeholder"])
			} else {
				assert.EqualValues(1, result.ErrorCounts["message_orphan_chat"])
				assert.Zero(result.SkippedCounts["reserved_placeholder"])
			}
		})
	}
}
