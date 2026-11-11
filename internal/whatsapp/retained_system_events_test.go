package whatsapp

import (
	"context"
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

func retainedEvent(t *testing.T, st *store.Store, phone, key string, raw waMessage, format string) (int64, int64) {
	t.Helper()
	requirements := require.New(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", phone)
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithTypeContext(t.Context(), source.ID, "old-group@g.us", "group_chat", "Historical group")
	requirements.NoError(err)
	id, err := st.UpsertMessageContext(t.Context(), &store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: key, MessageType: "whatsapp",
		SentAt: sql.NullTime{Time: time.UnixMilli(1700000000123).UTC(), Valid: true},
	})
	requirements.NoError(err)
	if format != "" {
		encoded, err := json.Marshal(raw)
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageRawWithFormatContext(t.Context(), id, encoded, format))
	}
	return id, source.ID
}

func retainedSystemMessage() waMessage {
	return waMessage{KeyID: "old-event", Timestamp: 1700000000123, MessageType: 112,
		SenderJIDRowID: sql.NullInt64{Int64: 0, Valid: true}}
}

func TestImportRepairsLegacySystemEventFromRetainedRaw(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+15550002222"
	raw := retainedSystemMessage()
	id, _ := retainedEvent(t, st, opts.Phone, raw.KeyID, raw, "whatsapp_json")
	requirements.NoError(st.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: `{"custom":"retained"}`, Valid: true}))
	requirements.NoError(st.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: "retained historical event text", Valid: true}, sql.NullString{}))
	beforeRaw, err := st.GetMessageRawContext(t.Context(), id)
	requirements.NoError(err)
	var encodedBefore []byte
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT raw_data FROM message_raw WHERE message_id=?`, id).Scan(&encodedBefore))
	other, _ := retainedEvent(t, st, "+15550003333", raw.KeyID, raw, "whatsapp_json")
	unknown, _ := retainedEvent(t, st, opts.Phone, "unknown-empty", waMessage{}, "")
	ordinary := raw
	ordinary.KeyID, ordinary.MessageType = "ordinary-empty", 0
	ordinaryID, _ := retainedEvent(t, st, opts.Phone, ordinary.KeyID, ordinary, "whatsapp_json")
	otherFormat := raw
	otherFormat.KeyID = "other-format"
	otherFormatID, _ := retainedEvent(t, st, opts.Phone, otherFormat.KeyID, otherFormat, "whatsapp_apple_json")
	backup := testBackup(t, modernRoster)
	first, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(first.Errors)
	assertions.EqualValues(1, first.SystemEventsClassified)
	assertions.EqualValues(2, first.MessagesProcessed, "historical repair must not inflate current snapshot coverage")
	metadata, err := st.GetMessageMetadataContext(t.Context(), id)
	requirements.NoError(err)
	var fields map[string]jsontext.Value
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	assertions.JSONEq(`"retained"`, string(fields["custom"]))
	assertions.JSONEq(`{"kind":"system_event","message_type":112,"timestamp_millis":1700000000123}`, string(fields["whatsapp_source_record"]))
	for _, unchanged := range []int64{other, unknown, ordinaryID, otherFormatID} {
		metadata, err := st.GetMessageMetadataContext(t.Context(), unchanged)
		requirements.NoError(err)
		assertions.False(metadata.Valid, "unproven or other-source row %d must remain unchanged", unchanged)
	}
	afterRaw, err := st.GetMessageRawContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal(beforeRaw, afterRaw)
	var encodedAfter []byte
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT raw_data FROM message_raw WHERE message_id=?`, id).Scan(&encodedAfter))
	assertions.Equal(encodedBefore, encodedAfter, "retain the exact compressed original")
	body, err := st.GetMessageBodyText(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("retained historical event text", body)
	var key, kind string
	var sent sql.NullTime
	var sender sql.NullInt64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT source_message_id,message_type,sent_at,sender_id FROM messages WHERE id=?`, id).Scan(&key, &kind, &sent, &sender))
	assertions.Equal(raw.KeyID, key)
	assertions.Equal("whatsapp", kind)
	assertions.Equal(raw.Timestamp, sent.Time.UnixMilli())
	assertions.False(sender.Valid)
	second, err := NewImporter(st, nil).Import(t.Context(), backup, opts)
	requirements.NoError(err)
	assertions.Zero(second.Errors)
	assertions.Zero(second.SystemEventsClassified)
	assertions.Zero(second.MessagesAdded)
}

func TestImportRejectsConflictingRetainedSystemEventEvidence(t *testing.T) {
	for _, mode := range []string{"key", "timestamp", "missing_date", "classification", "json", "compression", "encrypted"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			raw := retainedSystemMessage()
			switch mode {
			case "key":
				raw.KeyID = "wrong-key"
			case "timestamp":
				raw.Timestamp++
			}
			id, sourceID := retainedEvent(t, st, opts.Phone, "old-event", raw, "whatsapp_json")
			prior := sql.NullString{}
			switch mode {
			case "missing_date":
				_, err := st.DB().ExecContext(t.Context(), `UPDATE messages SET sent_at=NULL WHERE id=?`, id)
				requirements.NoError(err)
			case "classification":
				prior = sql.NullString{String: `{"whatsapp_source_record":{"kind":"human_message"}}`, Valid: true}
				requirements.NoError(st.SetMessageMetadataContext(t.Context(), id, prior))
			case "json":
				requirements.NoError(st.UpsertMessageRawWithFormatContext(t.Context(), id, []byte("not JSON"), "whatsapp_json"))
			case "compression":
				_, err := st.DB().ExecContext(t.Context(), `UPDATE message_raw SET raw_data=? WHERE message_id=?`, []byte("invalid zlib"), id)
				requirements.NoError(err)
			case "encrypted":
				_, err := st.DB().ExecContext(t.Context(), `UPDATE message_raw SET encryption_version=1 WHERE message_id=?`, id)
				requirements.NoError(err)
			}
			result, err := NewImporter(st, nil).Import(t.Context(), testBackup(t, modernRoster), opts)
			requirements.NoError(err)
			assertions.EqualValues(1, result.Errors)
			assertions.EqualValues(1, result.ErrorCounts["system_event_classification"])
			assertions.Zero(result.SystemEventsClassified)
			metadata, err := st.GetMessageMetadataContext(t.Context(), id)
			requirements.NoError(err)
			assertions.Equal(prior, metadata)
			var status string
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`, sourceID).Scan(&status))
			assertions.Equal("failed", status)
		})
	}
}

func TestImportPreservesConflictingRetainedAndCurrentSystemEvidence(t *testing.T) {
	for _, retainedType := range []int{0, 112} {
		t.Run(map[int]string{0: "current_system", 112: "retained_system"}[retainedType], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			opts := DefaultOptions()
			opts.Phone = "+15550002222"
			raw := retainedSystemMessage()
			raw.KeyID, raw.Timestamp, raw.MessageType = "message-1", 1700000000000, retainedType
			id, _ := retainedEvent(t, st, opts.Phone, raw.KeyID, raw, "whatsapp_json")
			_, err := st.DB().ExecContext(t.Context(), `UPDATE messages SET sent_at=? WHERE id=?`, time.UnixMilli(raw.Timestamp).UTC(), id)
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: "retained body", Valid: true}, sql.NullString{}))
			beforeRaw, err := st.GetMessageRawContext(t.Context(), id)
			requirements.NoError(err)
			sqlSetup := modernRoster
			if retainedType == 0 {
				sqlSetup += `UPDATE message SET message_type=112,text_data=NULL,sender_jid_row_id=0 WHERE _id=1;`
			}
			result, err := NewImporter(st, nil).Import(t.Context(), testBackup(t, sqlSetup), opts)
			requirements.NoError(err)
			assertions.EqualValues(1, result.Errors)
			assertions.EqualValues(1, result.ErrorCounts["system_event_classification"])
			assertions.EqualValues(1, result.SkippedCounts["system_event_conflict"])
			assertions.Zero(result.SystemEventsClassified)
			metadata, err := st.GetMessageMetadataContext(t.Context(), id)
			requirements.NoError(err)
			assertions.False(metadata.Valid)
			afterRaw, err := st.GetMessageRawContext(t.Context(), id)
			requirements.NoError(err)
			assertions.Equal(beforeRaw, afterRaw, "conflicting evidence must not be overwritten by the phone snapshot")
			body, err := st.GetMessageBodyText(t.Context(), id)
			requirements.NoError(err)
			assertions.Equal("retained body", body)
		})
	}
}

func TestRetainedSystemEventRepairHonorsCancellationAndSyncFences(t *testing.T) {
	for _, mode := range []string{"canceled", "superseded", "wrong_source"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			raw := retainedSystemMessage()
			id, sourceID := retainedEvent(t, st, "+15550002222", raw.KeyID, raw, "whatsapp_json")
			ownerID := sourceID
			if mode == "wrong_source" {
				other, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550003333")
				requirements.NoError(err)
				ownerID = other.ID
			}
			run, err := st.StartSyncContext(t.Context(), ownerID, "whatsapp_import")
			requirements.NoError(err)
			ctx := t.Context()
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if mode == "superseded" {
				requirements.NoError(st.FailSyncContext(t.Context(), run, "superseded"))
				_, err = st.StartSyncContext(t.Context(), sourceID, "whatsapp_import")
				requirements.NoError(err)
			}
			backup, err := openReadOnlyDatabase(t.Context(), testBackup(t, modernRoster))
			requirements.NoError(err)
			defer func() { assertions.NoError(backup.Close()) }()
			var observedErrors []error
			result, err := NewImporter(st.ScopedToSync(ownerID, run), nil).classifyRetainedSystemEvents(ctx, backup, sourceID, func(err error) {
				observedErrors = append(observedErrors, err)
			})
			if mode == "superseded" {
				requirements.NoError(err)
				requirements.Len(observedErrors, 1)
				requirements.ErrorIs(observedErrors[0], store.ErrSyncRunSuperseded)
			} else {
				requirements.Error(err)
			}
			assertions.Zero(result.classified)
			metadata, err := st.GetMessageMetadataContext(t.Context(), id)
			requirements.NoError(err)
			assertions.False(metadata.Valid)
		})
	}
}
