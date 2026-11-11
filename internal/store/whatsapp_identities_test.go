package store_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWhatsAppLIDWritesAreCanceledFencedAndAccountScoped(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	requirements.NoError(err)
	run, err := st.StartSyncContext(t.Context(), source.ID, "whatsapp_import")
	requirements.NoError(err)
	scoped := st.ScopedToSync(source.ID, run)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, phone := range []string{"", "+15550002222"} {
		_, err = scoped.EnsureWhatsAppLIDParticipantContext(canceled, source.ID, source.Identifier, "12345@lid", phone)
		requirements.ErrorIs(err, context.Canceled)
	}
	_, err = scoped.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, "+15550009999", "12345@lid", "")
	requirements.ErrorContains(err, "different source account")
	requirements.NoError(st.FailSyncContext(t.Context(), run, "superseded"))
	_, err = st.StartSyncContext(t.Context(), source.ID, "whatsapp_import")
	requirements.NoError(err)
	for _, phone := range []string{"", "+15550002222"} {
		_, err = scoped.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", phone)
		requirements.ErrorIs(err, store.ErrSyncRunSuperseded)
	}
	conversation, err := st.EnsureConversationWithTypeContext(t.Context(), source.ID, "test-group", "group_chat", "Test group")
	requirements.NoError(err)
	_, err = scoped.UpsertMessagePreservingExistingAttributionContext(t.Context(), &store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "stale-message", MessageType: "whatsapp",
	})
	requirements.ErrorIs(err, store.ErrSyncRunSuperseded)
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participants`).Scan(&count))
	assertions.Zero(count)
}

func TestWhatsAppLIDResolutionRejectsOtherIdentityEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	requirements.NoError(err)
	id, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
	requirements.NoError(err)
	requirements.NoError(st.SetParticipantIdentifierContext(t.Context(), id, "email", "curated@example.test"))
	_, err = st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "+15550002222")
	requirements.ErrorContains(err, "other participant identity evidence")
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participants`).Scan(&count))
	assertions.Equal(1, count, "a refused resolution rolls back the new phone participant too")
}

func TestWhatsAppReactionObservationRejectsWrongSourceCanceledAndSupersededWrites(t *testing.T) {
	for _, mode := range []string{"wrong_source", "canceled", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
			requirements.NoError(err)
			conversation, err := st.EnsureConversationWithTypeContext(t.Context(), source.ID, "group", "group_chat", "Test group")
			requirements.NoError(err)
			messageID := insertStoreTestMessage(t, st, source.ID, conversation, "message-1")
			participant, err := st.EnsureParticipantByPhone(t.Context(), "+15550002222", "", "whatsapp")
			requirements.NoError(err)
			requirements.NoError(st.ReplaceReactionsContext(t.Context(), messageID, []store.ReactionRef{
				{ParticipantID: participant, Type: "emoji", Value: "👍"},
			}))
			owner := source
			if mode == "wrong_source" {
				owner, err = st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550003333")
				requirements.NoError(err)
			}
			run, err := st.StartSyncContext(t.Context(), owner.ID, "whatsapp_import")
			requirements.NoError(err)
			scoped := st.ScopedToSync(owner.ID, run)
			ctx := t.Context()
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if mode == "superseded" {
				requirements.NoError(st.FailSyncContext(t.Context(), run, "superseded"))
				_, err = st.StartSyncContext(t.Context(), owner.ID, "whatsapp_import")
				requirements.NoError(err)
			}
			err = scoped.ObserveWhatsAppReactionContext(ctx, messageID, participant, "", 1700000000000)
			requirements.Error(err)
			switch mode {
			case "canceled":
				requirements.ErrorIs(err, context.Canceled)
			case "superseded":
				requirements.ErrorIs(err, store.ErrSyncRunSuperseded)
			}
			var value string
			requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT reaction_value FROM reactions WHERE message_id=?`), messageID).Scan(&value))
			assertions.Equal("👍", value)
		})
	}
}

func TestWhatsAppReactionOrderingBootstrapsAndPreservesOtherMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithTypeContext(t.Context(), source.ID, "group", "group_chat", "Test group")
	requirements.NoError(err)
	messageID := insertStoreTestMessage(t, st, source.ID, conversation, "message-1")
	participant, err := st.EnsureParticipantByPhone(t.Context(), "+15550002222", "", "whatsapp")
	requirements.NoError(err)
	requirements.NoError(st.UpsertReaction(t.Context(), messageID, participant, "emoji", "❤️", time.UnixMilli(1700000006000)))
	requirements.NoError(st.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{
		String: `{"custom":{"enabled":true},"whatsapp_reactions":null}`, Valid: true,
	}))
	requirements.NoError(st.ObserveWhatsAppReactionContext(t.Context(), messageID, participant, "👍", 1700000005000))
	err = st.ObserveWhatsAppReactionContext(t.Context(), messageID, participant, "👍", 1700000006000)
	requirements.ErrorContains(err, "same timestamp")
	var value string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reaction_value FROM reactions`).Scan(&value))
	assertions.Equal("❤️", value)
	requirements.NoError(st.ObserveWhatsAppReactionContext(t.Context(), messageID, participant, "", 1700000007000))
	requirements.NoError(st.ObserveWhatsAppReactionContext(t.Context(), messageID, participant, "👍", 1700000005000))
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions`).Scan(&count))
	assertions.Zero(count)
	metadata, err := st.GetMessageMetadataContext(t.Context(), messageID)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	assertions.Equal(map[string]any{"enabled": true}, fields["custom"])
	requirements.NoError(st.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{String: `null`, Valid: true}))
	requirements.NoError(st.ObserveWhatsAppReactionContext(t.Context(), messageID, participant, "👍", 1700000008000))
	requirements.NoError(st.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{String: `{"whatsapp_reactions":"invalid"}`, Valid: true}))
	err = st.ObserveWhatsAppReactionContext(t.Context(), messageID, participant, "", 1700000009000)
	requirements.ErrorContains(err, "reaction ordering")
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reaction_value FROM reactions`).Scan(&value))
	assertions.Equal("👍", value, "unreadable ordering metadata cannot authorize removal")
}
