package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPSTEnvelopeRepairFillsMissingFactsAndDirectParent(t *testing.T) {
	f := storetest.New(t)
	parent := f.CreateMessage("pst-parent")
	id := f.CreateMessage("pst-child")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("old")))
	_, err := f.Store.DB().Exec(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`, "parent@example.test", parent)
	require.NoError(t, err)
	date := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	env := &store.PSTRepairEnvelope{Subject: "Recovered topic", SentAt: date,
		MessageID: "child@example.test", InReplyTo: "parent@example.test", ThreadKey: "parent@example.test",
		From: []mime.Address{{Name: "Sender", Email: "sender@example.test"}},
		To:   []mime.Address{{Name: "Recipient", Email: "recipient@example.test"}}}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-child", []byte("old"), []byte("new"), nil, nil, env)
	require.NoError(t, err)
	assert.True(t, result.EnvelopeUpdated)
	assert.Contains(t, result.EnvelopePreserved, "conversation subject not changed")
	var conversationTitle string
	require.NoError(t, f.Store.DB().QueryRow(`SELECT title FROM conversations WHERE id = ?`, f.ConvID).Scan(&conversationTitle))
	assert.Equal(t, "Default Thread", conversationTitle)
	var subject, rfc string
	var sent time.Time
	var sender, gotParent int64
	require.NoError(t, f.Store.DB().QueryRow(`SELECT subject, sent_at, sender_id, rfc822_message_id, reply_to_message_id FROM messages WHERE id = ?`, id).Scan(&subject, &sent, &sender, &rfc, &gotParent))
	assert.Equal(t, "Recovered topic", subject)
	assert.True(t, date.Equal(sent))
	assert.NotZero(t, sender)
	assert.Equal(t, "child@example.test", rfc)
	assert.Equal(t, parent, gotParent)
	var from, to int
	require.NoError(t, f.Store.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, id).Scan(&from))
	require.NoError(t, f.Store.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`, id).Scan(&to))
	assert.Equal(t, 1, from)
	assert.Equal(t, 1, to)
	_, total, err := f.Store.SearchMessages("Recovered", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	result, err = f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-child", []byte("new"), []byte("new"), nil, nil, env)
	require.NoError(t, err)
	assert.False(t, result.EnvelopeUpdated)
	assert.Empty(t, result.EnvelopePreserved)
}

func TestPSTEnvelopeRepairPreservesConflictingFacts(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-conflict")
	oldParent := f.CreateMessage("pst-old-parent")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	keepDate := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	_, err := f.Store.DB().Exec(`UPDATE messages SET subject = ?, sent_at = ?, rfc822_message_id = ?, reply_to_message_id = ? WHERE id = ?`, "Keep subject", keepDate, "keep@example.test", oldParent, id)
	require.NoError(t, err)
	pid, err := f.Store.EnsureParticipant("keep@example.test", "Keep", "example.test")
	require.NoError(t, err)
	require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{pid}, []string{"Keep"}))
	env := &store.PSTRepairEnvelope{ExpectedSubject: "old subject", Subject: "new subject", SentAt: keepDate.Add(time.Hour),
		MessageID: "new@example.test", InReplyTo: "different@example.test",
		To: []mime.Address{{Email: "other@example.test"}}}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-conflict", []byte("raw"), []byte("raw"), nil, nil, env)
	require.NoError(t, err)
	assert.Subset(t, result.EnvelopePreserved, []string{"subject", "date", "to", "message-id"})
	var subject, rfc string
	var gotParent int64
	var sent time.Time
	require.NoError(t, f.Store.DB().QueryRow(`SELECT subject, sent_at, rfc822_message_id, reply_to_message_id FROM messages WHERE id = ?`, id).Scan(&subject, &sent, &rfc, &gotParent))
	assert.Equal(t, "Keep subject", subject)
	assert.True(t, keepDate.Equal(sent))
	assert.Equal(t, "keep@example.test", rfc)
	assert.Equal(t, oldParent, gotParent)
	var count int
	require.NoError(t, f.Store.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to' AND participant_id = ?`, id, pid).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestPSTEnvelopeRepairReportsUnresolvedSender(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-unresolved")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	env := &store.PSTRepairEnvelope{UnresolvedSender: true, From: []mime.Address{{Name: "No SMTP", Email: "local-only"}}}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-unresolved", []byte("raw"), []byte("raw"), nil, nil, env)
	require.NoError(t, err)
	assert.Contains(t, result.EnvelopePreserved, "unresolved sender")
	var sender sql.NullInt64
	require.NoError(t, f.Store.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, id).Scan(&sender))
	assert.False(t, sender.Valid)
}

func TestPSTEnvelopeRepairUsesMatchingExistingFromOrder(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-order")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	first, err := f.Store.EnsureParticipant("first@example.test", "First", "example.test")
	require.NoError(t, err)
	second, err := f.Store.EnsureParticipant("second@example.test", "Second", "example.test")
	require.NoError(t, err)
	require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{second, first}, []string{"Second", "First"}))
	env := &store.PSTRepairEnvelope{From: []mime.Address{{Email: "first@example.test"}, {Email: "second@example.test"}}}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-order", []byte("raw"), []byte("raw"), nil, nil, env)
	require.NoError(t, err)
	assert.True(t, result.EnvelopeUpdated)
	assert.Empty(t, result.EnvelopePreserved)
	var sender sql.NullInt64
	require.NoError(t, f.Store.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, id).Scan(&sender))
	assert.Equal(t, sql.NullInt64{Int64: first, Valid: true}, sender)
}

func TestPSTEnvelopeRepairKeepsUnresolvedSenderSearch(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-search-sender")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	pid, err := f.Store.EnsureParticipantByIdentifierContext(t.Context(), "pst-unresolved-sender", "synthetic:dn", "Shared Sendername")
	require.NoError(t, err)
	_, err = f.Store.DB().Exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, pid, id)
	require.NoError(t, err)
	require.NoError(t, f.Store.UpsertFTS(id, "old", "", "MessageSpecific Sendername", "", ""))
	env := &store.PSTRepairEnvelope{ExpectedSubject: "old", Subject: "new subject", UnresolvedSenderName: "MessageSpecific Sendername", UnresolvedSenderKey: "synthetic:dn"}
	_, err = f.Store.DB().Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "old", id)
	require.NoError(t, err)
	_, err = f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-search-sender", []byte("raw"), []byte("raw"), nil, nil, env)
	require.NoError(t, err)
	_, total, err := f.Store.SearchMessages("MessageSpecific", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
}

func TestPSTEnvelopeRepairPreservesConflictingReplyLink(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-reply-conflict")
	linked := f.CreateMessage("pst-linked")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	_, err := f.Store.DB().Exec(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`, "old-parent@example.test", linked)
	require.NoError(t, err)
	_, err = f.Store.DB().Exec(`UPDATE messages SET reply_to_message_id = ? WHERE id = ?`, linked, id)
	require.NoError(t, err)
	env := &store.PSTRepairEnvelope{InReplyTo: "new-parent@example.test", ThreadKey: "new-parent@example.test"}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-reply-conflict", []byte("raw"), []byte("raw"), nil, nil, env)
	require.NoError(t, err)
	assert.Contains(t, result.EnvelopePreserved, "reply link")
	var parent int64
	var metadata sql.NullString
	require.NoError(t, f.Store.DB().QueryRow(`SELECT reply_to_message_id, metadata FROM messages WHERE id = ?`, id).Scan(&parent, &metadata))
	assert.Equal(t, linked, parent)
	assert.NotContains(t, metadata.String, "new-parent@example.test")
}

func TestPSTEnvelopeRepairDoesNotLinkWithConflictingMessageID(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-id-conflict")
	parent := f.CreateMessage("pst-parent")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	_, err := f.Store.DB().Exec(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`, "parent@example.test", parent)
	require.NoError(t, err)
	_, err = f.Store.DB().Exec(`UPDATE messages SET rfc822_message_id = ?, metadata = ? WHERE id = ?`, "keep@example.test", `{"email_in_reply_to":"parent@example.test"}`, id)
	require.NoError(t, err)
	env := &store.PSTRepairEnvelope{MessageID: "other@example.test", InReplyTo: "parent@example.test"}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-id-conflict", []byte("raw"), []byte("raw"), nil, nil, env)
	require.NoError(t, err)
	assert.Contains(t, result.EnvelopePreserved, "message-id")
	var reply sql.NullInt64
	require.NoError(t, f.Store.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE id = ?`, id).Scan(&reply))
	assert.False(t, reply.Valid)
}

func TestPSTEnvelopeRepairReusesSenderWithDifferentCase(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-case")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	pid, err := f.Store.EnsureParticipant("Sender@Example.test", "Sender", "example.test")
	require.NoError(t, err)
	_, err = f.Store.DB().Exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, pid, id)
	require.NoError(t, err)
	env := &store.PSTRepairEnvelope{From: []mime.Address{{Name: "Sender", Email: "sender@example.test"}}}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-case", []byte("raw"), []byte("raw"), nil, nil, env)
	require.NoError(t, err)
	assert.True(t, result.EnvelopeUpdated)
	var sender, recipient int64
	require.NoError(t, f.Store.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, id).Scan(&sender))
	require.NoError(t, f.Store.DB().QueryRow(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, id).Scan(&recipient))
	assert.Equal(t, pid, sender)
	assert.Equal(t, pid, recipient)
}

func TestPSTEnvelopeRepairReportsUnavailableFromAndUnresolvedRecipients(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-unavailable")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	pid, err := f.Store.EnsureParticipant("known@example.test", "Known", "example.test")
	require.NoError(t, err)
	_, err = f.Store.DB().Exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, pid, id)
	require.NoError(t, err)
	env := &store.PSTRepairEnvelope{UnresolvedRecipients: []string{"to", "to-unresolved", "cc-unresolved", "bcc-unresolved", "recipient metadata unreadable"}}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-unavailable", []byte("raw"), []byte("raw"), nil, nil, env)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"from unavailable", "to-unresolved", "cc-unresolved", "bcc-unresolved", "recipient metadata unreadable"}, result.EnvelopePreserved)
}
