package importer

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pstreader "go.kenn.io/msgvault/internal/pst"
	"go.kenn.io/msgvault/internal/store"
)

type pstEnvelopeRepairFixture struct {
	store       *store.Store
	source      *store.Source
	entry       *pstreader.MessageEntry
	messageID   int64
	parentID    int64
	labelID     int64
	dir         string
	sourceMsgID string
	oldRaw      []byte
}

func newPSTEnvelopeRepairFixture(t *testing.T) pstEnvelopeRepairFixture {
	t.Helper()
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	label, err := st.EnsureLabel(src.ID, "label", "Archive", "user")
	require.NoError(t, err)
	dir := t.TempDir()
	parentRaw := []byte("From: parent@example.test\r\nTo: owner@example.test\r\nSubject: Parent\r\nMessage-ID: <parent@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nParent body\r\n")
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-archive-parent", "parent-hash", parentRaw, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{"pst-archive-parent"})
	require.NoError(t, err)
	parentID := ids["pst-archive-parent"]
	require.NotZero(t, parentID)
	headers := "Microsoft Mail Internet Headers Version 2.0\r\n" +
		"From: Original Sender <sender@example.test>\r\n" +
		"To: Owner <owner@example.test>\r\nCc: Reviewer <reviewer@example.test>\r\n" +
		"Subject: Original subject\r\nDate: Mon, 01 Jan 2024 12:34:56 +0000\r\n" +
		"Message-ID: <child@example.test>\r\nIn-Reply-To: <parent@example.test>\r\nReferences: <parent@example.test>\r\n"
	entry := &pstreader.MessageEntry{EntryID: "321", PSTArchiveID: "archive", TransportHeaders: headers, BodyText: "Original searchable content"}
	correct, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	mimeStart := bytes.Index(correct, []byte("MIME-Version:"))
	require.NotEqual(t, -1, mimeStart)
	old := append([]byte(headers), correct[mimeStart:]...)
	const sourceMsgID = "pst-archive-321"
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, []int64{label}, sourceMsgID, "old-hash", old, time.Time{}, slog.Default()))
	ids, err = st.MessageExistsBatch(src.ID, []string{sourceMsgID})
	require.NoError(t, err)
	messageID := ids[sourceMsgID]
	require.NotZero(t, messageID)
	return pstEnvelopeRepairFixture{st, src, entry, messageID, parentID, label, dir, sourceMsgID, old}
}

func TestRepairPSTMessageRecoversMissingEnvelopeAndReplyFacts(t *testing.T) {
	f := newPSTEnvelopeRepairFixture(t)
	// Model an old malformed-header ingest that lost the envelope facts.
	_, err := f.store.DB().Exec(`UPDATE messages SET sender_id = NULL, subject = NULL, sent_at = NULL,
		rfc822_message_id = NULL, metadata = NULL, reply_to_message_id = NULL WHERE id = ?`, f.messageID)
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`DELETE FROM message_recipients WHERE message_id = ?`, f.messageID)
	require.NoError(t, err)
	// A reply already in the archive is waiting for this message's ID.
	replyRaw := []byte("From: sender@example.test\r\nTo: owner@example.test\r\nSubject: Reply\r\nMessage-ID: <later@example.test>\r\nIn-Reply-To: <child@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nReply body\r\n")
	require.NoError(t, pstMessageIngester(nil)(t.Context(), f.store, f.source.ID, f.source.Identifier, f.dir, nil, "pst-archive-later", "later-hash", replyRaw, time.Time{}, slog.Default()))
	ids, err := f.store.MessageExistsBatch(f.source.ID, []string{"pst-archive-later"})
	require.NoError(t, err)
	replyID := ids["pst-archive-later"]
	require.NotZero(t, replyID)
	var replyBefore sql.NullInt64
	var replyConversation int64
	require.NoError(t, f.store.DB().QueryRow(`SELECT reply_to_message_id, conversation_id FROM messages WHERE id = ?`, replyID).Scan(&replyBefore, &replyConversation))
	require.False(t, replyBefore.Valid)
	var targetConversation int64
	require.NoError(t, f.store.DB().QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, f.messageID).Scan(&targetConversation))
	result, err := repairPSTMessage(t.Context(), f.store, f.source.ID, f.sourceMsgID, f.messageID, f.entry, f.dir, slog.Default())
	require.NoError(t, err)
	assert.Contains(t, result.EnvelopePreserved, "child reply links not reconciled")
	var sender, subject, rfcID, metadata string
	var sent sql.NullTime
	var parent sql.NullInt64
	require.NoError(t, f.store.DB().QueryRow(`SELECT COALESCE(p.email_address, ''), COALESCE(m.subject, ''), m.sent_at,
		COALESCE(m.rfc822_message_id, ''), COALESCE(CAST(m.metadata AS TEXT), ''), m.reply_to_message_id
		FROM messages m LEFT JOIN participants p ON p.id = m.sender_id WHERE m.id = ?`, f.messageID).Scan(&sender, &subject, &sent, &rfcID, &metadata, &parent))
	assert.Equal(t, "sender@example.test", sender)
	assert.Equal(t, "Original subject", subject)
	require.True(t, sent.Valid)
	assert.Equal(t, time.Date(2024, 1, 1, 12, 34, 56, 0, time.UTC), sent.Time.UTC())
	assert.Equal(t, "child@example.test", rfcID)
	assert.Contains(t, metadata, "parent@example.test")
	require.True(t, parent.Valid)
	assert.Equal(t, f.parentID, parent.Int64)
	var toCount, ccCount int
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to' AND email_address = ?`, f.messageID, "owner@example.test").Scan(&toCount))
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'cc' AND email_address = ?`, f.messageID, "reviewer@example.test").Scan(&ccCount))
	assert.Equal(t, 1, toCount)
	assert.Equal(t, 1, ccCount)
	var replyAfter sql.NullInt64
	var gotReplyConversation, gotTargetConversation int64
	require.NoError(t, f.store.DB().QueryRow(`SELECT reply_to_message_id, conversation_id FROM messages WHERE id = ?`, replyID).Scan(&replyAfter, &gotReplyConversation))
	assert.False(t, replyAfter.Valid, "repair must not scan or alter another message's reply link")
	assert.Equal(t, replyConversation, gotReplyConversation)
	require.NoError(t, f.store.DB().QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, f.messageID).Scan(&gotTargetConversation))
	assert.Equal(t, targetConversation, gotTargetConversation)
	var count int
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`, f.messageID, f.labelID).Scan(&count))
	assert.Equal(t, 1, count)
	// A repeated repair must not duplicate facts or alter the message identity.
	_, err = repairPSTMessage(t.Context(), f.store, f.source.ID, f.sourceMsgID, f.messageID, f.entry, f.dir, slog.Default())
	require.NoError(t, err)
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to' AND email_address = ?`, f.messageID, "owner@example.test").Scan(&toCount))
	assert.Equal(t, 1, toCount)
}

func TestRepairPSTMessagePreservesExistingEnvelopeAndReply(t *testing.T) {
	f := newPSTEnvelopeRepairFixture(t)
	customRaw := []byte("From: custom@example.test\r\nTo: owner@example.test\r\nSubject: Other parent\r\nMessage-ID: <other-parent@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nOther\r\n")
	require.NoError(t, pstMessageIngester(nil)(t.Context(), f.store, f.source.ID, f.source.Identifier, f.dir, nil, "pst-archive-other", "other-hash", customRaw, time.Time{}, slog.Default()))
	ids, err := f.store.MessageExistsBatch(f.source.ID, []string{"pst-archive-other"})
	require.NoError(t, err)
	otherID := ids["pst-archive-other"]
	require.NotZero(t, otherID)
	customID, err := f.store.EnsureParticipant("custom@example.test", "Custom Sender", "example.test")
	require.NoError(t, err)
	customDate := time.Date(2019, 2, 3, 4, 5, 6, 0, time.UTC)
	var originalConversation int64
	require.NoError(t, f.store.DB().QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, f.messageID).Scan(&originalConversation))
	_, err = f.store.DB().Exec(`UPDATE messages SET sender_id = ?, subject = ?, sent_at = ?, rfc822_message_id = ?,
		metadata = ?, reply_to_message_id = ? WHERE id = ?`, customID, "User subject", customDate, "custom-id@example.test", `{"email_in_reply_to":"other-parent@example.test","custom":"keep"}`, otherID, f.messageID)
	require.NoError(t, err)
	// A user-provided recipient for an already populated role must survive.
	_, err = f.store.DB().Exec(`DELETE FROM message_recipients WHERE message_id = ?`, f.messageID)
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address)
		VALUES (?, ?, 'to', 'Custom Sender', 'custom@example.test')`, f.messageID, customID)
	require.NoError(t, err)
	_, err = repairPSTMessage(t.Context(), f.store, f.source.ID, f.sourceMsgID, f.messageID, f.entry, f.dir, slog.Default())
	require.NoError(t, err)
	var gotSender, gotParent, gotConversation int64
	var gotSubject, gotRFC, metadata string
	var gotDate time.Time
	require.NoError(t, f.store.DB().QueryRow(`SELECT sender_id, subject, sent_at, rfc822_message_id, CAST(metadata AS TEXT), reply_to_message_id, conversation_id FROM messages WHERE id = ?`, f.messageID).Scan(&gotSender, &gotSubject, &gotDate, &gotRFC, &metadata, &gotParent, &gotConversation))
	assert.Equal(t, customID, gotSender)
	assert.Equal(t, "User subject", gotSubject)
	assert.Equal(t, customDate, gotDate.UTC())
	assert.Equal(t, "custom-id@example.test", gotRFC)
	assert.Contains(t, metadata, "other-parent@example.test")
	assert.Contains(t, metadata, `"custom":"keep"`)
	assert.Equal(t, otherID, gotParent)
	assert.Equal(t, originalConversation, gotConversation)
	var count int
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to' AND email_address = 'custom@example.test'`, f.messageID).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`, f.messageID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestRepairPSTMessageDoesNotChooseAmbiguousReplyParent(t *testing.T) {
	f := newPSTEnvelopeRepairFixture(t)
	// Duplicate IDs in the same source do not identify a unique parent.
	duplicate := []byte("From: parent@example.test\r\nTo: owner@example.test\r\nSubject: Duplicate parent\r\nMessage-ID: <parent@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nParent body\r\n")
	require.NoError(t, pstMessageIngester(nil)(t.Context(), f.store, f.source.ID, f.source.Identifier, f.dir, nil, "pst-archive-parent-duplicate", "duplicate-hash", duplicate, time.Time{}, slog.Default()))
	_, err := f.store.DB().Exec(`UPDATE messages SET rfc822_message_id = NULL, metadata = NULL, reply_to_message_id = NULL WHERE id = ?`, f.messageID)
	require.NoError(t, err)
	_, err = repairPSTMessage(t.Context(), f.store, f.source.ID, f.sourceMsgID, f.messageID, f.entry, f.dir, slog.Default())
	require.NoError(t, err)
	var parent sql.NullInt64
	var metadata string
	require.NoError(t, f.store.DB().QueryRow(`SELECT reply_to_message_id, COALESCE(CAST(metadata AS TEXT), '') FROM messages WHERE id = ?`, f.messageID).Scan(&parent, &metadata))
	assert.False(t, parent.Valid)
	assert.Contains(t, metadata, "parent@example.test")
}

func TestRepairPSTMessageReportsUnresolvedExchangeSenderWithoutInventingAddress(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	headers := "Microsoft Mail Internet Headers Version 2.0\r\nSubject: Original subject\r\n"
	identity := &pstreader.UnresolvedIdentity{Name: "Exchange Sender", AddressType: "EX", RawAddress: "/o=Example/ou=Unit/cn=Recipients/cn=Sender"}
	entry := &pstreader.MessageEntry{EntryID: "987", PSTArchiveID: "archive", TransportHeaders: headers, SenderName: identity.Name, UnresolvedSender: identity, BodyText: "Body"}
	correct, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	metadataStart := bytes.Index(correct, []byte("X-Msgvault-Unresolved-Sender:"))
	require.NotEqual(t, -1, metadataStart)
	old := append([]byte(headers), correct[metadataStart:]...)
	const sourceMsgID = "pst-archive-987"
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMsgID, "old-hash", old, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMsgID})
	require.NoError(t, err)
	messageID := ids[sourceMsgID]
	require.NotZero(t, messageID)
	var sender sql.NullInt64
	require.NoError(t, st.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, messageID).Scan(&sender))
	require.False(t, sender.Valid)
	result, err := repairPSTMessage(t.Context(), st, src.ID, sourceMsgID, messageID, entry, dir, slog.Default())
	require.NoError(t, err)
	assert.Contains(t, result.EnvelopePreserved, "unresolved sender")
	require.NoError(t, st.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, messageID).Scan(&sender))
	assert.False(t, sender.Valid)
	var fromCount int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, messageID).Scan(&fromCount))
	assert.Zero(t, fromCount)
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Empty(t, message.Header.Get("From"))
	encoded := strings.Join(strings.Fields(message.Header.Get("X-Msgvault-Unresolved-Sender")), "")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	require.NoError(t, err)
	var preserved struct {
		Name        string `json:"name"`
		AddressType string `json:"address_type"`
		Address     string `json:"address"`
		SMTP        string `json:"smtp"`
	}
	require.NoError(t, json.Unmarshal(decoded, &preserved))
	assert.Equal(t, identity.Name, preserved.Name)
	assert.Equal(t, identity.AddressType, preserved.AddressType)
	assert.Equal(t, identity.RawAddress, preserved.Address)
	assert.Empty(t, preserved.SMTP)
}

func TestRepairPSTMessageKeepsUnresolvedSenderSearchable(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	headers := "Microsoft Mail Internet Headers Version 2.0\r\nSubject: Original subject\r\n"
	identity := &pstreader.UnresolvedIdentity{Name: "ExchangeNameNeedle", AddressType: "EX", RawAddress: "/o=Example/ou=Unit/cn=Recipients/cn=Sender"}
	entry := &pstreader.MessageEntry{EntryID: "988", PSTArchiveID: "archive", TransportHeaders: headers, SenderName: identity.Name, UnresolvedSender: identity, BodyText: "Actual searchable message body"}
	correct, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	metadataStart := bytes.Index(correct, []byte("X-Msgvault-Unresolved-Sender:"))
	require.NotEqual(t, -1, metadataStart)
	old := append([]byte(headers), correct[metadataStart:]...)
	const sourceMsgID = "pst-archive-988"
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMsgID, "old-hash", old, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMsgID})
	require.NoError(t, err)
	messageID := ids[sourceMsgID]
	require.NotZero(t, messageID)
	resolved := unresolvedPSTSender(correct)
	require.NotNil(t, resolved)
	participant, err := st.EnsureParticipantByIdentifierContext(t.Context(), "pst-unresolved-sender", resolved.participantKey(src.ID, sourceMsgID), "Different Shared Display")
	require.NoError(t, err)
	_, err = st.DB().Exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, participant, messageID)
	require.NoError(t, err)
	// Simulate a previously indexed name-only participant with no From row.
	_, err = st.DB().Exec(`DELETE FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, messageID)
	require.NoError(t, err)
	body, err := st.GetMessageBodyText(messageID)
	require.NoError(t, err)
	require.NoError(t, st.UpsertFTS(messageID, "Original subject", body, identity.Name, "", ""))
	_, count, err := st.SearchMessages(identity.Name, 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	result, err := repairPSTMessage(t.Context(), st, src.ID, sourceMsgID, messageID, entry, dir, slog.Default())
	require.NoError(t, err)
	assert.True(t, result.BodyUpdated, "repair must exercise the search-index rebuild")
	_, count, err = st.SearchMessages(identity.Name, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	var storedID int64
	require.NoError(t, st.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, messageID).Scan(&storedID))
	assert.Equal(t, participant, storedID)
}

func TestRepairPSTMessageRestoresSenderFromReorderedExistingFromRows(t *testing.T) {
	f := newPSTEnvelopeRepairFixture(t)
	before := "From: Original Sender <sender@example.test>\r\n"
	after := "From: First <first@example.test>, Second <second@example.test>\r\n"
	entry := *f.entry
	entry.TransportHeaders = strings.Replace(entry.TransportHeaders, before, after, 1)
	require.NotEqual(t, f.entry.TransportHeaders, entry.TransportHeaders)
	old := bytes.Replace(f.oldRaw, []byte(before), []byte(after), 1)
	require.NoError(t, f.store.UpsertMessageRaw(f.messageID, old))
	firstID, err := f.store.EnsureParticipant("first@example.test", "First", "example.test")
	require.NoError(t, err)
	secondID, err := f.store.EnsureParticipant("second@example.test", "Second", "example.test")
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`UPDATE messages SET sender_id = NULL WHERE id = ?`, f.messageID)
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`DELETE FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, f.messageID)
	require.NoError(t, err)
	for _, participant := range []struct {
		id    int64
		email string
	}{{secondID, "second@example.test"}, {firstID, "first@example.test"}} {
		_, err = f.store.DB().Exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
			VALUES (?, ?, 'from', ?)`, f.messageID, participant.id, participant.email)
		require.NoError(t, err)
	}
	_, err = repairPSTMessage(t.Context(), f.store, f.source.ID, f.sourceMsgID, f.messageID, &entry, f.dir, slog.Default())
	require.NoError(t, err)
	var sender sql.NullInt64
	require.NoError(t, f.store.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, f.messageID).Scan(&sender))
	require.True(t, sender.Valid)
	assert.Equal(t, firstID, sender.Int64)
	rows, err := f.store.DB().Query(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'from' ORDER BY id`, f.messageID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var got []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		got = append(got, id)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []int64{secondID, firstID}, got)
}

func TestRepairPSTMessageReportsUnavailableSender(t *testing.T) {
	f := newPSTEnvelopeRepairFixture(t)
	from := "From: Original Sender <sender@example.test>\r\n"
	entry := *f.entry
	entry.TransportHeaders = strings.Replace(entry.TransportHeaders, from, "", 1)
	old := bytes.Replace(f.oldRaw, []byte(from), nil, 1)
	require.NoError(t, f.store.UpsertMessageRaw(f.messageID, old))
	_, err := f.store.DB().Exec(`UPDATE messages SET sender_id = NULL WHERE id = ?`, f.messageID)
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`DELETE FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, f.messageID)
	require.NoError(t, err)
	result, err := repairPSTMessage(t.Context(), f.store, f.source.ID, f.sourceMsgID, f.messageID, &entry, f.dir, slog.Default())
	require.NoError(t, err)
	assert.Contains(t, result.EnvelopePreserved, "sender unavailable")
	var sender sql.NullInt64
	require.NoError(t, f.store.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, f.messageID).Scan(&sender))
	assert.False(t, sender.Valid)
}

func TestRepairPSTMessageReportsUnresolvedRecipientWithoutInventingAddress(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	headers := "Microsoft Mail Internet Headers Version 2.0\r\nFrom: sender@example.test\r\nTo: valid@example.test\r\nSubject: Original subject\r\n"
	identity := pstreader.MessageRecipient{Name: "Unknown\r\nBcc: injected@example.test", Role: 1, Unresolved: true, AddressType: "EX", RawAddress: "/o=Example/ou=Unit/cn=Recipients/cn=Unknown"}
	entry := &pstreader.MessageEntry{EntryID: "989", PSTArchiveID: "archive", TransportHeaders: headers, Recipients: []pstreader.MessageRecipient{identity}, BodyText: "Body"}
	correct, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	metadataStart := bytes.Index(correct, []byte("X-Msgvault-Unresolved-Recipient-Name:"))
	require.NotEqual(t, -1, metadataStart)
	old := append([]byte(headers), correct[metadataStart:]...)
	const sourceMsgID = "pst-archive-989"
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMsgID, "old-hash", old, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMsgID})
	require.NoError(t, err)
	messageID := ids[sourceMsgID]
	require.NotZero(t, messageID)
	result, err := repairPSTMessage(t.Context(), st, src.ID, sourceMsgID, messageID, entry, dir, slog.Default())
	require.NoError(t, err)
	assert.Contains(t, result.EnvelopePreserved, "to-unresolved")
	var validCount, totalCount, bccCount int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to' AND email_address = ?`, messageID, "valid@example.test").Scan(&validCount))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`, messageID).Scan(&totalCount))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'bcc'`, messageID).Scan(&bccCount))
	assert.Equal(t, 1, validCount)
	assert.Equal(t, 1, totalCount)
	assert.Zero(t, bccCount)
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Empty(t, message.Header.Get("Bcc"))
	encoded := strings.Join(strings.Fields(message.Header.Get("X-Msgvault-Unresolved-Recipient")), "")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	require.NoError(t, err)
	var preserved struct {
		Role        int32  `json:"role"`
		Name        string `json:"name"`
		AddressType string `json:"address_type"`
		Address     string `json:"address"`
		SMTP        string `json:"smtp"`
	}
	require.NoError(t, json.Unmarshal(decoded, &preserved))
	assert.Equal(t, identity.Role, preserved.Role)
	assert.Equal(t, identity.Name, preserved.Name)
	assert.Equal(t, identity.AddressType, preserved.AddressType)
	assert.Equal(t, identity.RawAddress, preserved.Address)
	assert.Empty(t, preserved.SMTP)
}
