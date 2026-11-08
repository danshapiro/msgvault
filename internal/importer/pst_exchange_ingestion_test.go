package importer

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
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

// Exercise extraction, MIME construction, archive ingestion, and reply linking
// together; the repository's PST fixtures do not contain these Exchange values.
func TestPSTUnresolvedExchangeIdentitiesSurviveIngestionAndThreading(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	senderName, senderDN := "Sender Name", "/O=EXAMPLE/CN=SENDER"
	authorName, authorDN := "Author Name", "/O=EXAMPLE/CN=AUTHOR"
	addressType := "EX"
	body := "Unique body for the unresolved Exchange identity"
	attachment := []byte("synthetic attachment bytes")
	ids := make(map[string]int64)

	// Import reply first, matching a PST where the parent appears later.
	for _, item := range []struct{ id, parent string }{{"reply", "parent"}, {"parent", ""}} {
		messageID := "<" + item.id + "@example.test>"
		inReplyTo := ""
		if item.parent != "" {
			inReplyTo = "<" + item.parent + "@example.test>"
		}
		headers := "From: transport@example.test\r\nTo: recipient@example.test\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\nSubject: Exchange test\r\n"
		entry := pstreader.ExtractMessage(&pstlib.Message{Properties: &properties.Message{
			SenderName: &senderName, SenderAddressType: &addressType, SenderEmailAddress: &senderDN,
			SentRepresentingName: &authorName, SentRepresentingAddressType: &addressType, SentRepresentingEmailAddress: &authorDN,
			TransportMessageHeaders: &headers, InternetMessageId: &messageID, InReplyToId: &inReplyTo, Body: &body,
		}}, "Inbox")
		require.NotNil(t, entry)
		require.NoError(t, entry.HeaderError)
		require.NoError(t, entry.BodyError)
		raw, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: "evidence.txt", MIMEType: "text/plain", Content: attachment}})
		require.NoError(t, err)
		sourceID := "pst-exchange-" + item.id
		require.NoError(t, IngestRawMessage(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceID, item.id, raw, time.Time{}, slog.Default()))
		require.NoError(t, recordPstMessageHeaders(t.Context(), st, src.ID, sourceID, raw))
		var id int64
		require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, sourceID).Scan(&id))
		ids[item.id] = id
		stored, err := st.GetMessageRaw(id)
		require.NoError(t, err)
		assert.Equal(t, raw, stored)
		message, err := mail.ReadMessage(bytes.NewReader(stored))
		require.NoError(t, err)
		for _, identity := range []struct{ role, name, address string }{{"Sender", senderName, senderDN}, {"Author", authorName, authorDN}} {
			encoded := strings.Join(strings.Fields(message.Header.Get("X-Msgvault-Unresolved-"+identity.role)), "")
			decoded, err := base64.RawURLEncoding.DecodeString(encoded)
			require.NoError(t, err)
			var metadata map[string]string
			require.NoError(t, json.Unmarshal(decoded, &metadata))
			assert.Equal(t, identity.name, metadata["name"])
			assert.Equal(t, "EX", metadata["address_type"])
			assert.Equal(t, identity.address, metadata["address"])
		}
		parsed, err := msgmime.Parse(stored)
		require.NoError(t, err)
		require.Len(t, parsed.From, 1)
		assert.Equal(t, "transport@example.test", parsed.From[0].Email)
		require.Len(t, parsed.To, 1)
		assert.Equal(t, "recipient@example.test", parsed.To[0].Email)
		assert.Equal(t, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), parsed.Date)
		assert.Equal(t, body+"\r\n", parsed.BodyText)
		require.Len(t, parsed.Attachments, 1)
		assert.Equal(t, attachment, parsed.Attachments[0].Content)
		storedBody, err := st.GetMessageBodyText(id)
		require.NoError(t, err)
		assert.Equal(t, body+"\r\n", storedBody)
		refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
		require.NoError(t, err)
		require.Len(t, refs, 1)
		assert.Equal(t, "evidence.txt", refs[0].Filename)
		storedAttachment, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
		require.NoError(t, err)
		assert.Equal(t, attachment, storedAttachment)
	}

	require.NoError(t, finishPstThreads(t.Context(), st, src.ID))
	var parent sql.NullInt64
	var replyConversation, parentConversation int64
	require.NoError(t, st.DB().QueryRow(`SELECT reply_to_message_id, conversation_id FROM messages WHERE id = ?`, ids["reply"]).Scan(&parent, &replyConversation))
	require.NoError(t, st.DB().QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, ids["parent"]).Scan(&parentConversation))
	assert.True(t, parent.Valid)
	assert.Equal(t, ids["parent"], parent.Int64)
	assert.Equal(t, parentConversation, replyConversation)
}
