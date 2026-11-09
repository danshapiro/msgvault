package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIntegrityWritesHonorContextAndSourceScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	participantID := f.EnsureParticipant("member@example.test", "Member", "example.test")
	other, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15555550101")
	require.NoError(err)
	otherConversation, err := st.EnsureConversationWithTypeContext(t.Context(), other.ID, "other-chat", "group_chat", "Other")
	require.NoError(err)
	runID, err := st.StartSyncContext(t.Context(), f.Source.ID, "full")
	require.NoError(err)
	scoped := st.ScopedToSync(f.Source.ID, runID)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	msg := &store.Message{SourceID: other.ID, ConversationID: otherConversation, SourceMessageID: "scoped-message", MessageType: "whatsapp"}
	_, err = scoped.UpsertMessagePreservingAttributionContext(t.Context(), msg)
	require.ErrorContains(err, "is scoped to source")
	require.ErrorContains(scoped.SetConversationParticipantRoleContext(t.Context(), otherConversation, participantID, "admin"), "is scoped to source")
	msg.SourceID, msg.ConversationID = f.Source.ID, f.ConvID
	_, err = scoped.UpsertMessagePreservingAttributionContext(canceled, msg)
	require.ErrorIs(err, context.Canceled)
	require.ErrorIs(scoped.SetConversationParticipantRoleContext(canceled, f.ConvID, participantID, "admin"), context.Canceled)
	var messages, members int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&messages))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_participants`).Scan(&members))
	assert.Zero(messages)
	assert.Zero(members)

	_, err = scoped.UpsertMessagePreservingAttributionContext(t.Context(), msg)
	require.NoError(err)
	require.NoError(scoped.SetConversationParticipantRoleContext(t.Context(), f.ConvID, participantID, "admin"))
	require.NoError(scoped.SetConversationParticipantRoleContext(t.Context(), f.ConvID, participantID, "member"))
	var role string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT role FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`), f.ConvID, participantID).Scan(&role))
	assert.Equal("member", role)
}
