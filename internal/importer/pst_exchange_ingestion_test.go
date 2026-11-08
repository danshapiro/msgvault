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
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
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

// A readable sender must survive the full ingest and search paths even when
// Exchange supplied no SMTP address. None of the names appears in the body or
// subject, so a search hit must come from the sender itself.
func TestPSTUnresolvedExchangeSenderNameSearch(t *testing.T) {
	for _, tc := range []struct {
		name, sender, author, headers, expectedName, expectedEmail, searchTerm string
	}{
		{name: "sender without transport headers", sender: "Sender Zucchini", expectedName: "Sender Zucchini", searchTerm: "Zucchini"},
		{name: "author with unusable transport from", author: "Author Kumquat", headers: "From: invalid <\r\n", expectedName: "Author Kumquat", searchTerm: "Kumquat"},
		{name: "represented author takes precedence", sender: "Sender Zucchini", author: "Author Kumquat", expectedName: "Author Kumquat", searchTerm: "Kumquat"},
		{name: "sender name controls remain safe", sender: "Sender Quince\r\nX-Injected: evil", expectedName: "Sender QuinceX-Injected: evil", searchTerm: "QuinceX"},
		{name: "valid transport from takes precedence", sender: "Sender Zucchini", author: "Author Kumquat", headers: "From: Transport Person <transportunique@example.test>\r\n", expectedName: "Transport Person", expectedEmail: "transportunique@example.test", searchTerm: "transportunique"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openIntegrationStore(t)
			require.True(t, st.FTS5Available())
			// PST supports a caller-selected source type, not only "pst".
			src, err := st.GetOrCreateSource("custom-pst", "owner@example.test")
			require.NoError(t, err)
			exchange, dn, body := "EX", "/O=EXAMPLE/CN=PERSON", "Ordinary message body"
			props := &properties.Message{TransportMessageHeaders: &tc.headers, Body: &body}
			if tc.sender != "" {
				props.SenderName, props.SenderAddressType, props.SenderEmailAddress = &tc.sender, &exchange, &dn
			}
			if tc.author != "" {
				props.SentRepresentingName, props.SentRepresentingAddressType, props.SentRepresentingEmailAddress = &tc.author, &exchange, &dn
			}
			entry := pstreader.ExtractMessage(&pstlib.Message{Properties: props}, "Inbox")
			require.NoError(t, entry.HeaderError)
			raw, err := pstreader.BuildRFC5322(entry, nil)
			require.NoError(t, err)
			require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, "", nil, "pst-no-smtp", "hash", raw, time.Time{}, slog.Default()))

			rows, total, err := st.SearchMessages(tc.searchTerm, 0, 10)
			require.NoError(t, err)
			require.Equal(t, int64(1), total)
			require.Len(t, rows, 1)
			if tc.expectedEmail == "" {
				assert.Equal(t, tc.expectedName, rows[0].From)
			} else {
				assert.Contains(t, rows[0].From, tc.expectedEmail)
			}
			assert.Equal(t, tc.expectedName, rows[0].FromName)
			assert.Equal(t, tc.expectedEmail, rows[0].FromEmail)
			engine := query.NewSQLiteEngine(st.DB())
			checkQueryEngine := func() {
				for _, fast := range []bool{false, true} {
					var results []query.MessageSummary
					var searchErr error
					if fast {
						results, searchErr = engine.SearchFast(t.Context(), search.Parse(tc.searchTerm), query.MessageFilter{}, 10, 0)
					} else {
						results, searchErr = engine.Search(t.Context(), search.Parse(tc.searchTerm), 10, 0)
					}
					require.NoError(t, searchErr)
					require.Len(t, results, 1, "fast=%t", fast)
					assert.Equal(t, rows[0].ID, results[0].ID)
					assert.Equal(t, tc.expectedName, results[0].FromName)
					assert.Equal(t, tc.expectedEmail, results[0].FromEmail)
				}
			}
			checkQueryEngine()
			detail, err := engine.GetMessage(t.Context(), rows[0].ID)
			require.NoError(t, err)
			require.NotNil(t, detail)
			require.Len(t, detail.From, 1)
			assert.Equal(t, tc.expectedName, detail.From[0].Name)
			assert.Equal(t, tc.expectedEmail, detail.From[0].Email)
			stored, err := st.GetMessageRaw(rows[0].ID)
			require.NoError(t, err)
			message, err := mail.ReadMessage(bytes.NewReader(stored))
			require.NoError(t, err)
			assert.Empty(t, message.Header.Get("X-Injected"))
			if strings.Contains(tc.sender, "\r\n") {
				encoded := strings.Join(strings.Fields(message.Header.Get("X-Msgvault-Unresolved-Sender")), "")
				decoded, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
				require.NoError(t, decodeErr)
				var metadata map[string]string
				require.NoError(t, json.Unmarshal(decoded, &metadata))
				assert.Equal(t, tc.sender, metadata["name"])
			}
			parsed, err := msgmime.Parse(stored)
			require.NoError(t, err)
			for _, address := range parsed.From {
				assert.Equal(t, tc.expectedEmail, address.Email)
			}
			_, err = st.RebuildFTS(nil)
			require.NoError(t, err)
			resultsAfterRebuild, total, err := st.SearchMessages(tc.searchTerm, 0, 10)
			require.NoError(t, err)
			require.Equal(t, int64(1), total)
			require.Len(t, resultsAfterRebuild, 1)
			assert.Equal(t, rows[0].ID, resultsAfterRebuild[0].ID)
			checkQueryEngine()
		})
	}
}
