package whatsapp

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportClassifiesOnlySourceProvenExistingLegacySystemEvents(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, modernRoster+`
UPDATE message SET message_type=112,text_data=NULL,sender_jid_row_id=0 WHERE _id=1;
INSERT INTO message VALUES(3,10,0,'new-system-event',0,1700000002000,112,NULL,0,0);`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", opts.Phone)
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithTypeContext(t.Context(), source.ID, "12345@g.us", "group_chat", "Synthetic group")
	requirements.NoError(err)
	legacy, err := st.UpsertMessageContext(t.Context(), &store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "message-1", MessageType: "whatsapp",
		SentAt: sql.NullTime{Time: time.UnixMilli(1700000000000).UTC(), Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadataContext(t.Context(), legacy, sql.NullString{String: `{"custom":"retained"}`, Valid: true}))
	unknown, err := st.UpsertMessageContext(t.Context(), &store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "unknown-old-empty", MessageType: "whatsapp",
	})
	requirements.NoError(err)
	imp := NewImporter(st, nil)
	first, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(first.Errors)
	assertions.EqualValues(1, first.MessagesAdded)
	assertions.EqualValues(1, first.SystemEventsClassified)
	assertions.EqualValues(2, first.SkippedCounts["type_112"])
	metadata, err := st.GetMessageMetadataContext(t.Context(), legacy)
	requirements.NoError(err)
	var fields map[string]jsontext.Value
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	assertions.JSONEq(`"retained"`, string(fields["custom"]))
	assertions.JSONEq(`{"kind":"system_event","message_type":112,"timestamp_millis":1700000000000}`, string(fields["whatsapp_source_record"]))
	unknownMetadata, err := st.GetMessageMetadataContext(t.Context(), unknown)
	requirements.NoError(err)
	assertions.False(unknownMetadata.Valid)
	var newEvents int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE source_message_id='new-system-event'`).Scan(&newEvents))
	assertions.Zero(newEvents, "the repair annotates history rather than inserting skipped event records")

	second, err := imp.Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(second.Errors)
	assertions.Zero(second.MessagesAdded)
	assertions.Zero(second.SystemEventsClassified)
	repeatedMetadata, err := st.GetMessageMetadataContext(t.Context(), legacy)
	requirements.NoError(err)
	assertions.Equal(metadata, repeatedMetadata)
}

func TestImportKeepsConflictingLegacySystemClassificationPartial(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, modernRoster+`UPDATE message SET message_type=112,text_data=NULL,sender_jid_row_id=0 WHERE _id=1;`)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", opts.Phone)
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithTypeContext(t.Context(), source.ID, "12345@g.us", "group_chat", "Synthetic group")
	requirements.NoError(err)
	legacy, err := st.UpsertMessageContext(t.Context(), &store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "message-1", MessageType: "whatsapp",
		SentAt: sql.NullTime{Time: time.UnixMilli(1700000000001).UTC(), Valid: true},
	})
	requirements.NoError(err)
	result, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.EqualValues(1, result.Errors)
	assertions.EqualValues(1, result.ErrorCounts["system_event_classification"])
	assertions.Zero(result.SystemEventsClassified)
	metadata, err := st.GetMessageMetadataContext(t.Context(), legacy)
	requirements.NoError(err)
	assertions.False(metadata.Valid)
	var status string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT status FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), source.ID).Scan(&status))
	assertions.Equal("failed", status)
}
