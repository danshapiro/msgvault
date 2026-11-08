package importer

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	msgmime "go.kenn.io/msgvault/internal/mime"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

func TestRepairPSTMessagePreservesArchiveAndRetriesMissingAttachments(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	label, err := st.EnsureLabel(src.ID, "archive-label", "Archive", "user")
	require.NoError(t, err)
	dir := t.TempDir()
	headers := "Microsoft Mail Internet Headers Version 2.0\r\n" +
		"From: sender@example.test\r\nTo: owner@example.test\r\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\r\nSubject: Synthetic repair\r\n" +
		"Message-ID: <parent@example.test>\r\n"
	rtf := pstTestMELA([]byte(`{\rtf1\ansi original searchablebodytoken}`))
	entry := pstreader.ExtractMessage(&pstlib.Message{
		Identifier: 321, Properties: &properties.Message{TransportMessageHeaders: &headers},
		PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{{ID: 0x1009, Type: pstlib.PropertyTypeBinary, Data: rtf}}},
	}, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	require.NoError(t, entry.BodyError)
	require.NotNil(t, entry.RawRTFAttachment)
	ordinary := []byte{0, 1, 2, 0xff, 0x80, 4}
	corrected, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{
		{Filename: "ordinary.bin", MIMEType: "application/octet-stream", Content: ordinary},
		{Filename: "empty.bin", MIMEType: "application/octet-stream", Content: []byte{}},
	})
	require.NoError(t, err)
	// The old generator copied the invalid Outlook preamble and source fields,
	// then appended its generated MIME headers and body.
	mimeStart := bytes.Index(corrected, []byte("MIME-Version:"))
	require.NotEqual(t, -1, mimeStart)
	old := append([]byte(headers), corrected[mimeStart:]...)
	const sourceMessageID = "pst-synthetic-archive-321"
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, []int64{label}, sourceMessageID, "old", old, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMessageID})
	require.NoError(t, err)
	messageID := ids[sourceMessageID]
	require.NotZero(t, messageID)
	initialRefs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	require.Empty(t, initialRefs)
	// Seed a preexisting unrelated attachment to verify that repair is additive.
	existing := &msgmime.Attachment{Filename: "existing.bin", ContentType: "application/octet-stream", Content: []byte("already stored"), PartKey: "unrelated"}
	require.NoError(t, storeAttachment(st, dir, messageID, existing, false))
	var existingID int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM attachments WHERE message_id = ? AND filename = ?`, messageID, "existing.bin").Scan(&existingID))

	// Preserve a reply that was linked before repair.
	require.NoError(t, st.RecordPstEmailHeadersContext(t.Context(), src.ID, messageID, "parent@example.test", "", "parent@example.test"))
	reply := []byte("From: sender@example.test\r\nTo: owner@example.test\r\nSubject: Re: Synthetic repair\r\nMessage-ID: <reply@example.test>\r\nIn-Reply-To: <parent@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nReply body\r\n")
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-synthetic-archive-reply", "reply", reply, time.Time{}, slog.Default()))
	require.NoError(t, st.ResolveEmailReplyParentsContext(t.Context(), src.ID, 0, nil))
	require.NoError(t, st.ReconcilePstEmailThreadsContext(t.Context(), src.ID))
	var replyID, conversationID, parentID int64
	require.NoError(t, st.DB().QueryRow(`SELECT id, conversation_id, reply_to_message_id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-synthetic-archive-reply").Scan(&replyID, &conversationID, &parentID))
	require.Equal(t, messageID, parentID)
	beforeRaw, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	separator := bytes.Index(beforeRaw, []byte("\r\n\r\n"))
	require.NotEqual(t, -1, separator)
	oldBody := bytes.Clone(beforeRaw[separator:])
	// A mistaken source identity must not replace a different message.
	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID+"-wrong", messageID, entry, dir, slog.Default())
	require.Error(t, err)
	unchanged, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	assert.Equal(t, beforeRaw, unchanged)
	wrongEntry := *entry
	wrongEntry.TransportHeaders = "A different invalid preamble\r\nFrom: sender@example.test\r\n"
	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, &wrongEntry, dir, slog.Default())
	require.Error(t, err)
	unchanged, err = st.GetMessageRaw(messageID)
	require.NoError(t, err)
	assert.Equal(t, beforeRaw, unchanged)

	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default())
	require.NoError(t, err)
	newRaw, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	separator = bytes.Index(newRaw, []byte("\r\n\r\n"))
	require.NotEqual(t, -1, separator)
	assert.Equal(t, oldBody, newRaw[separator:], "original MIME body and boundary must remain byte-identical")
	message, err := mail.ReadMessage(bytes.NewReader(newRaw))
	require.NoError(t, err)
	oldHeader, _, found := bytes.Cut(beforeRaw, []byte("\r\n\r\n"))
	require.True(t, found)
	for name, want := range map[string][]byte{
		"X-Msgvault-Original-Transport-Headers": []byte(headers),
		"X-Msgvault-Original-Stored-Headers":    oldHeader,
	} {
		value := strings.Join(strings.Fields(message.Header.Get(name)), "")
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(value)
		require.NoError(t, decodeErr)
		assert.Equal(t, want, decoded)
	}
	wrongEntry = *entry
	wrongEntry.TransportHeaders += "X-Other-Source: different\r\n"
	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, &wrongEntry, dir, slog.Default())
	require.Error(t, err)
	unchanged, err = st.GetMessageRaw(messageID)
	require.NoError(t, err)
	assert.Equal(t, newRaw, unchanged)
	parsed, err := msgmime.Parse(newRaw)
	require.NoError(t, err)
	require.Len(t, parsed.Attachments, 3)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	require.Len(t, refs, 4)
	want := map[string][]byte{"ordinary.bin": ordinary, "empty.bin": {}, "original-pst-rtf-compressed.bin": rtf, "existing.bin": existing.Content}
	for _, ref := range refs {
		content, readErr := os.ReadFile(filepath.Join(dir, ref.StoragePath))
		require.NoError(t, readErr)
		assert.Equal(t, want[ref.Filename], content)
	}
	var existingAfter, labelCount, gotParent, gotConversation int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM attachments WHERE message_id = ? AND filename = ?`, messageID, "existing.bin").Scan(&existingAfter))
	assert.Equal(t, existingID, existingAfter)
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`, messageID, label).Scan(&labelCount))
	assert.Equal(t, int64(1), labelCount)
	require.NoError(t, st.DB().QueryRow(`SELECT reply_to_message_id, conversation_id FROM messages WHERE id = ?`, replyID).Scan(&gotParent, &gotConversation))
	assert.Equal(t, messageID, gotParent)
	assert.Equal(t, conversationID, gotConversation)
	afterBody, err := st.GetMessageBodyText(messageID)
	require.NoError(t, err)
	assert.Contains(t, afterBody, "searchablebodytoken")
	_, total, err := st.SearchMessages("searchablebodytoken", 0, 10)
	require.NoError(t, err)
	assert.Positive(t, total)

	// Simulate an earlier attempt that committed headers but missed an attachment.
	_, err = st.DB().Exec(`DELETE FROM attachments WHERE message_id = ? AND filename = ?`, messageID, "ordinary.bin")
	require.NoError(t, err)
	for range 2 {
		_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default())
		require.NoError(t, err)
	}
	refs, err = st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	assert.Len(t, refs, 4)
	var count int64
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, sourceMessageID).Scan(&count))
	assert.Equal(t, int64(1), count)
}

func TestRepairPSTMessageReportsBlockedAttachmentAndRetries(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	headers := "Microsoft Mail Internet Headers Version 2.0\r\nFrom: sender@example.test\r\nSubject: blocked\r\n"
	body := "searchable body"
	entry := pstreader.ExtractMessage(&pstlib.Message{
		Identifier: 322, Properties: &properties.Message{TransportMessageHeaders: &headers, Body: &body},
	}, "Inbox")
	require.NotNil(t, entry)
	content := []byte("source attachment survives storage denial")
	corrected, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: "blocked.bin", MIMEType: "application/octet-stream", Content: content}})
	require.NoError(t, err)
	mimeStart := bytes.Index(corrected, []byte("MIME-Version:"))
	require.NotEqual(t, -1, mimeStart)
	old := append([]byte(headers), corrected[mimeStart:]...)
	const sourceMessageID = "pst-synthetic-archive-322"
	dir := t.TempDir()
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMessageID, "old", old, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMessageID})
	require.NoError(t, err)
	messageID := ids[sourceMessageID]
	require.NotZero(t, messageID)
	verifyErr := errors.New("PST source changed")
	_, err = repairPSTMessageWithVerify(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default(), func() error { return verifyErr })
	require.ErrorIs(t, err, verifyErr)
	unchanged, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	assert.Equal(t, old, unchanged)
	beforeRefs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	assert.Empty(t, beforeRefs)
	// A body independently changed since import must never be overwritten by
	// the recovery of the MIME body.
	require.NoError(t, st.UpsertMessageBody(messageID, sql.NullString{String: "independentbodytoken", Valid: true}, sql.NullString{}))
	require.NoError(t, st.UpsertFTS(messageID, "blocked", "independentbodytoken", "sender@example.test", "", ""))
	blockedDir := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blockedDir, []byte("file"), 0600))
	_, err = repairPSTMessageWithVerify(t.Context(), st, src.ID, sourceMessageID, messageID, entry, blockedDir, slog.Default(), func() error { return verifyErr })
	require.ErrorIs(t, err, verifyErr)
	require.ErrorContains(t, err, "blocked.bin")
	unchanged, err = st.GetMessageRaw(messageID)
	require.NoError(t, err)
	assert.Equal(t, old, unchanged)
	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, blockedDir, slog.Default())
	require.Error(t, err)
	require.ErrorContains(t, err, "blocked.bin")
	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default())
	require.NoError(t, err)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	got, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
	require.NoError(t, err)
	assert.Equal(t, content, got)
	preservedBody, err := st.GetMessageBodyText(messageID)
	require.NoError(t, err)
	assert.Equal(t, "independentbodytoken", preservedBody)
	_, total, err := st.SearchMessages("independentbodytoken", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
}

func TestRepairPSTMessageDoesNotOverwriteConflictingAttachment(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	headers := "Microsoft Mail Internet Headers Version 2.0\r\nFrom: sender@example.test\r\nSubject: conflict\r\n"
	body := "body"
	entry := pstreader.ExtractMessage(&pstlib.Message{
		Identifier: 323, Properties: &properties.Message{TransportMessageHeaders: &headers, Body: &body},
	}, "Inbox")
	require.NotNil(t, entry)
	corrected, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: "same.bin", MIMEType: "application/octet-stream", Content: []byte("source bytes")}})
	require.NoError(t, err)
	parsed, err := msgmime.Parse(corrected)
	require.NoError(t, err)
	require.Len(t, parsed.Attachments, 1)
	require.NotEmpty(t, parsed.Attachments[0].PartKey)
	mimeStart := bytes.Index(corrected, []byte("MIME-Version:"))
	require.NotEqual(t, -1, mimeStart)
	old := append([]byte(headers), corrected[mimeStart:]...)
	const sourceMessageID = "pst-synthetic-archive-323"
	dir := t.TempDir()
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMessageID, "old", old, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMessageID})
	require.NoError(t, err)
	messageID := ids[sourceMessageID]
	require.NotZero(t, messageID)
	existingContent := []byte("different existing bytes")
	existing := &msgmime.Attachment{Filename: "same.bin", ContentType: "application/octet-stream", Content: existingContent, PartKey: parsed.Attachments[0].PartKey}
	require.NoError(t, storeAttachment(st, dir, messageID, existing, false))
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	existingRef := refs[0]
	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default())
	require.Error(t, err)
	refs, err = st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, existingRef.ContentHash, refs[0].ContentHash)
	assert.Equal(t, existingRef.StoragePath, refs[0].StoragePath)
	got, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
	require.NoError(t, err)
	assert.Equal(t, existingContent, got)
}

func TestRepairPSTMessageReportsMalformedMIMEPart(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	headers := "Microsoft Mail Internet Headers Version 2.0\r\nFrom: sender@example.test\r\nSubject: damaged part\r\n"
	body := "body"
	entry := pstreader.ExtractMessage(&pstlib.Message{
		Identifier: 324, Properties: &properties.Message{TransportMessageHeaders: &headers, Body: &body},
	}, "Inbox")
	require.NotNil(t, entry)
	content := []byte("uniquely encoded attachment")
	corrected, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: "damaged.bin", MIMEType: "application/octet-stream", Content: content}})
	require.NoError(t, err)
	encoded := []byte(base64.StdEncoding.EncodeToString(content))
	require.True(t, bytes.Contains(corrected, encoded))
	corrected = bytes.Replace(corrected, encoded, []byte("not-valid-base64%%%"), 1)
	mimeStart := bytes.Index(corrected, []byte("MIME-Version:"))
	require.NotEqual(t, -1, mimeStart)
	old := append([]byte(headers), corrected[mimeStart:]...)
	const sourceMessageID = "pst-synthetic-archive-324"
	dir := t.TempDir()
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMessageID, "old", old, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMessageID})
	require.NoError(t, err)
	messageID := ids[sourceMessageID]
	require.NotZero(t, messageID)
	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default())
	require.Error(t, err)
	unchanged, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	assert.Equal(t, old, unchanged)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestRepairPSTMessageRefusesUnrelatedValidMessage(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	headers := "From: sender@example.test\r\nSubject: valid message\r\n"
	body := "original body"
	entry := pstreader.ExtractMessage(&pstlib.Message{
		Identifier: 325, Properties: &properties.Message{TransportMessageHeaders: &headers, Body: &body},
	}, "Inbox")
	require.NotNil(t, entry)
	raw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	const sourceMessageID = "pst-synthetic-archive-325"
	dir := t.TempDir()
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMessageID, "original", raw, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMessageID})
	require.NoError(t, err)
	messageID := ids[sourceMessageID]
	require.NotZero(t, messageID)
	_, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default())
	require.Error(t, err)
	unchanged, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	assert.Equal(t, raw, unchanged)
}
