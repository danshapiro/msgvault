package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestWhatsAppRetainedSystemScanIsScopedPagedAndDoesNotReadBodies(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newWhatsAppSystemEventFixture(t)
	raw := []byte(`{"key_id":"legacy-event","message_type":112}`)
	requirements.NoError(f.store.UpsertMessageRawWithFormatContext(t.Context(), f.messageID, raw, "whatsapp_json"))
	otherSource, err := f.store.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550003333")
	requirements.NoError(err)
	otherConversation, err := f.store.EnsureConversationWithTypeContext(t.Context(), otherSource.ID, "other-group", "group_chat", "Other group")
	requirements.NoError(err)
	add := func(source, conversation int64, key, format string, sender sql.NullInt64) int64 {
		id, err := f.store.UpsertMessageContext(t.Context(), &store.Message{
			SourceID: source, ConversationID: conversation, SourceMessageID: key, MessageType: "whatsapp", SenderID: sender,
		})
		requirements.NoError(err)
		if format != "" {
			requirements.NoError(f.store.UpsertMessageRawWithFormatContext(t.Context(), id, raw, format))
		}
		return id
	}
	add(otherSource.ID, otherConversation, "other-source", "whatsapp_json", sql.NullInt64{})
	add(f.sourceID, f.conversationID, "no-raw", "", sql.NullInt64{})
	add(f.sourceID, f.conversationID, "other-format", "whatsapp_apple_json", sql.NullInt64{})
	participant, err := f.store.EnsureParticipantByPhone(t.Context(), "+15550004444", "Known sender", "whatsapp")
	requirements.NoError(err)
	add(f.sourceID, f.conversationID, "known-sender", "whatsapp_json", sql.NullInt64{Int64: participant, Valid: true})
	last := add(f.sourceID, f.conversationID, "last-candidate", "whatsapp_json", sql.NullInt64{})

	// Deny actual SQLite body-table reads: a body join would make this real
	// production query fail, without checking implementation text.
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	requirements.NoError(err)
	requirements.NoError(conn.Raw(func(driver any) error {
		sqliteConn, ok := driver.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected SQLite test driver %T", driver)
		}
		sqliteConn.RegisterAuthorizer(func(operation int, table, _, _ string) int {
			if operation == sqlite3.SQLITE_READ && table == "message_bodies" {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	requirements.NoError(conn.Close())
	t.Cleanup(func() {
		conn, err := f.store.DB().Conn(context.WithoutCancel(t.Context()))
		if assertions.NoError(err) {
			assertions.NoError(conn.Raw(func(driver any) error {
				sqliteConn, ok := driver.(*sqlite3.SQLiteConn)
				if !ok {
					return fmt.Errorf("unexpected SQLite test driver %T", driver)
				}
				sqliteConn.RegisterAuthorizer(nil)
				return nil
			}))
			assertions.NoError(conn.Close())
		}
	})
	first, err := f.store.ScanWhatsAppSystemEventCandidatesContext(t.Context(), f.sourceID, 0, 1)
	requirements.NoError(err)
	requirements.Len(first, 1)
	assertions.Equal(f.messageID, first[0].MessageID)
	assertions.Equal(raw, first[0].RawData)
	requirements.NoError(first[0].RawDecodeError)
	second, err := f.store.ScanWhatsAppSystemEventCandidatesContext(t.Context(), f.sourceID, first[0].MessageID, 1)
	requirements.NoError(err)
	requirements.Len(second, 1)
	assertions.Equal(last, second[0].MessageID)
	end, err := f.store.ScanWhatsAppSystemEventCandidatesContext(t.Context(), f.sourceID, last, 1)
	requirements.NoError(err)
	assertions.Empty(end)
}

func TestWhatsAppRetainedSystemScanHonorsCancellationAndSourceScope(t *testing.T) {
	requirements := require.New(t)
	f := newWhatsAppSystemEventFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.store.ScanWhatsAppSystemEventCandidatesContext(ctx, f.sourceID, 0, 10)
	requirements.ErrorIs(err, context.Canceled)
	run, err := f.store.StartSyncContext(t.Context(), f.sourceID, "whatsapp_import")
	requirements.NoError(err)
	_, err = f.store.ScopedToSync(f.sourceID, run).ScanWhatsAppSystemEventCandidatesContext(t.Context(), f.sourceID+1, 0, 10)
	requirements.Error(err)
	for _, bounds := range [][3]int64{{0, 0, 10}, {f.sourceID, -1, 10}, {f.sourceID, 0, 0}, {f.sourceID, 0, 1001}} {
		_, err := f.store.ScanWhatsAppSystemEventCandidatesContext(t.Context(), bounds[0], bounds[1], int(bounds[2]))
		requirements.Error(err)
	}
}
