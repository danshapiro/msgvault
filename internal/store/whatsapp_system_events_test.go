package store_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type whatsAppSystemEventFixture struct {
	store          *store.Store
	sourceID       int64
	conversationID int64
	messageID      int64
	timestamp      int64
}

func newWhatsAppSystemEventFixture(t *testing.T) whatsAppSystemEventFixture {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithTypeContext(t.Context(), source.ID, "group", "group_chat", "Test group")
	requirements.NoError(err)
	timestamp := int64(1700000000123)
	message, err := st.UpsertMessageContext(t.Context(), &store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "legacy-event", MessageType: "whatsapp",
		SentAt: sql.NullTime{Time: time.UnixMilli(timestamp).UTC(), Valid: true},
	})
	requirements.NoError(err)
	return whatsAppSystemEventFixture{st, source.ID, conversation, message, timestamp}
}

func TestWhatsAppSystemEventClassificationPreservesHistoryAndIsReversible(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newWhatsAppSystemEventFixture(t)
	prior := sql.NullString{String: `{"custom":{"enabled":true},"whatsapp_reactions":{"actor":{"timestamp":4,"value":"👍"}}}`, Valid: true}
	requirements.NoError(f.store.SetMessageMetadataContext(t.Context(), f.messageID, prior))
	body := sql.NullString{String: "retained old event text", Valid: true}
	requirements.NoError(f.store.UpsertMessageBodyContext(t.Context(), f.messageID, body, sql.NullString{}))
	hash := strings.Repeat("a", 64)
	requirements.NoError(f.store.UpsertAttachmentRecord(t.Context(), f.messageID, store.AttachmentWrite{
		Filename: "old.txt", MIMEType: "text/plain", StoragePath: "aa/" + hash, ContentHash: hash,
		Size: 4, SourcePartKey: "legacy:1", State: attachmentpolicy.StateStored,
	}))
	unknown, err := f.store.UpsertMessageContext(t.Context(), &store.Message{
		SourceID: f.sourceID, ConversationID: f.conversationID, SourceMessageID: "unknown-empty", MessageType: "whatsapp",
	})
	requirements.NoError(err)

	changed, err := f.store.ObserveWhatsAppSystemEventContext(t.Context(), f.sourceID, f.conversationID, "legacy-event", 112, f.timestamp)
	requirements.NoError(err)
	assertions.True(changed)
	metadata, err := f.store.GetMessageMetadataContext(t.Context(), f.messageID)
	requirements.NoError(err)
	var fields map[string]jsontext.Value
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	assertions.JSONEq(`{"enabled":true}`, string(fields["custom"]))
	assertions.JSONEq(`{"actor":{"timestamp":4,"value":"👍"}}`, string(fields["whatsapp_reactions"]))
	assertions.JSONEq(`{"kind":"system_event","message_type":112,"timestamp_millis":1700000000123}`, string(fields["whatsapp_source_record"]))

	var key, kind string
	var sent sql.NullTime
	var sender sql.NullInt64
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT source_message_id,message_type,sent_at,sender_id FROM messages WHERE id=?`), f.messageID).Scan(&key, &kind, &sent, &sender))
	assertions.Equal("legacy-event", key)
	assertions.Equal("whatsapp", kind)
	assertions.True(sent.Time.Equal(time.UnixMilli(f.timestamp)))
	assertions.False(sender.Valid)
	var storedBody string
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), f.messageID).Scan(&storedBody))
	assertions.Equal(body.String, storedBody)
	attachments, err := f.store.MessageMIMEAttachmentsContext(t.Context(), f.messageID)
	requirements.NoError(err)
	requirements.Len(attachments, 1)
	assertions.Equal(hash, attachments[0].ContentHash)
	unknownMetadata, err := f.store.GetMessageMetadataContext(t.Context(), unknown)
	requirements.NoError(err)
	assertions.False(unknownMetadata.Valid, "absence of source evidence must not classify another empty historical record")

	changed, err = f.store.ObserveWhatsAppSystemEventContext(t.Context(), f.sourceID, f.conversationID, "legacy-event", 112, f.timestamp)
	requirements.NoError(err)
	assertions.False(changed, "same observation is idempotent")
	requirements.NoError(f.store.SetMessageMetadataContext(t.Context(), f.messageID, prior))
	restored, err := f.store.GetMessageMetadataContext(t.Context(), f.messageID)
	requirements.NoError(err)
	assertions.Equal(prior, restored, "ordinary metadata restoration reverses only the classification")
}

func TestWhatsAppSystemEventClassificationRejectsUnmatchedOrConflictingEvidence(t *testing.T) {
	for _, mode := range []string{"wrong_conversation", "wrong_timestamp", "missing_timestamp", "wrong_message_type", "unsupported_source_type", "conflicting_classification", "malformed_classification", "invalid_metadata", "wrong_source_kind"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newWhatsAppSystemEventFixture(t)
			conversation, timestamp, sourceType := f.conversationID, f.timestamp, 112
			prior := sql.NullString{}
			switch mode {
			case "wrong_conversation":
				conversation++
			case "wrong_timestamp":
				timestamp++
			case "missing_timestamp":
				timestamp = 0
			case "wrong_message_type":
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET message_type='teams' WHERE id=?`), f.messageID)
				requirements.NoError(err)
			case "unsupported_source_type":
				sourceType = 0
			case "conflicting_classification":
				prior = sql.NullString{String: `{"whatsapp_source_record":{"kind":"human_message","message_type":0,"timestamp_millis":1700000000123}}`, Valid: true}
			case "malformed_classification":
				prior = sql.NullString{String: `{"whatsapp_source_record":"invalid"}`, Valid: true}
			case "invalid_metadata":
				prior = sql.NullString{String: `[]`, Valid: true}
			case "wrong_source_kind":
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE sources SET source_type='teams' WHERE id=?`), f.sourceID)
				requirements.NoError(err)
			}
			if prior.Valid {
				requirements.NoError(f.store.SetMessageMetadataContext(t.Context(), f.messageID, prior))
			}
			changed, err := f.store.ObserveWhatsAppSystemEventContext(t.Context(), f.sourceID, conversation, "legacy-event", sourceType, timestamp)
			requirements.Error(err)
			assertions.False(changed)
			metadata, err := f.store.GetMessageMetadataContext(t.Context(), f.messageID)
			requirements.NoError(err)
			assertions.Equal(prior, metadata)
		})
	}
}

func TestWhatsAppSystemEventClassificationDoesNotInsertMissingSourceMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newWhatsAppSystemEventFixture(t)
	changed, err := f.store.ObserveWhatsAppSystemEventContext(t.Context(), f.sourceID, f.conversationID, "not-archived", 112, f.timestamp)
	requirements.NoError(err)
	assertions.False(changed)
	var count int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&count))
	assertions.Equal(1, count)
}

func TestWhatsAppSystemEventClassificationHonorsCancellationAndSyncFences(t *testing.T) {
	for _, mode := range []string{"canceled", "superseded", "wrong_sync_source"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newWhatsAppSystemEventFixture(t)
			ownerID := f.sourceID
			if mode == "wrong_sync_source" {
				other, err := f.store.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550002222")
				requirements.NoError(err)
				ownerID = other.ID
			}
			run, err := f.store.StartSyncContext(t.Context(), ownerID, "whatsapp_import")
			requirements.NoError(err)
			scoped := f.store.ScopedToSync(ownerID, run)
			ctx := t.Context()
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if mode == "superseded" {
				requirements.NoError(f.store.FailSyncContext(t.Context(), run, "superseded"))
				_, err = f.store.StartSyncContext(t.Context(), ownerID, "whatsapp_import")
				requirements.NoError(err)
			}
			changed, err := scoped.ObserveWhatsAppSystemEventContext(ctx, f.sourceID, f.conversationID, "legacy-event", 112, f.timestamp)
			requirements.Error(err)
			assertions.False(changed)
			if mode == "canceled" {
				requirements.ErrorIs(err, context.Canceled)
			}
			if mode == "superseded" {
				requirements.ErrorIs(err, store.ErrSyncRunSuperseded)
			}
			metadata, err := f.store.GetMessageMetadataContext(t.Context(), f.messageID)
			requirements.NoError(err)
			assertions.False(metadata.Valid)
		})
	}
}

func TestWhatsAppSystemEventClassificationAcceptsEmptyMetadata(t *testing.T) {
	for _, prior := range []sql.NullString{{}, {String: "null", Valid: true}} {
		t.Run(prior.String, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newWhatsAppSystemEventFixture(t)
			requirements.NoError(f.store.SetMessageMetadataContext(t.Context(), f.messageID, prior))
			changed, err := f.store.ObserveWhatsAppSystemEventContext(t.Context(), f.sourceID, f.conversationID, "legacy-event", 112, f.timestamp)
			requirements.NoError(err)
			assertions.True(changed)
			metadata, err := f.store.GetMessageMetadataContext(t.Context(), f.messageID)
			requirements.NoError(err)
			assertions.JSONEq(`{"whatsapp_source_record":{"kind":"system_event","message_type":112,"timestamp_millis":1700000000123}}`, metadata.String)
		})
	}
}
