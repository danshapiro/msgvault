package importer

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/mail"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

func TestUnresolvedPSTRecipientWarningOmitsAddress(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	entry := &pstreader.MessageEntry{EntryID: "entry-42", Recipients: []pstreader.MessageRecipient{
		{Name: "Private Name", Role: 2, Unresolved: true, AddressType: "EX", RawAddress: "/O=PRIVATE/CN=PERSON"},
		{Name: "Valid", Role: 1, Email: "valid@example.test"},
	}}
	logUnresolvedPSTRecipients(log, entry)
	var record map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record))
	assert.Equal(t, "WARN", record["level"])
	assert.Equal(t, "entry-42", record["entry_id"])
	assert.Equal(t, float64(2), record["role"])
	assert.NotContains(t, logs.String(), "Private Name")
	assert.NotContains(t, logs.String(), "/O=PRIVATE/CN=PERSON")
	assert.NotContains(t, logs.String(), "valid@example.test")
}

func TestUnresolvedPSTRecipientSurvivesIngestion(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	entry := &pstreader.MessageEntry{
		SenderEmail: "sender@example.test", Subject: "Synthetic message", BodyText: "uniquebodytoken",
		Recipients: []pstreader.MessageRecipient{{Name: "Unresolved Person", Role: 1, Unresolved: true, AddressType: "EX", RawAddress: "/O=EXAMPLE/CN=PERSON"}},
	}
	raw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	require.NoError(t, IngestRawMessage(t.Context(), st, src.ID, "owner@example.test", "", nil, "pst-unresolved", "hash", raw, time.Time{}, slog.Default()))
	var messageID int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = ?`, "pst-unresolved").Scan(&messageID))
	stored, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	parsed, err := mail.ReadMessage(bytes.NewReader(stored))
	require.NoError(t, err)
	assert.NotEmpty(t, parsed.Header.Get("X-Msgvault-Unresolved-Recipient"))
	assert.Empty(t, parsed.Header.Get("To"), "do not invent an email address")
	assert.Contains(t, parsed.Header.Get("X-Msgvault-Unresolved-Recipient-Name"), "Unresolved Person")
	var recipients int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type IN ('to', 'cc', 'bcc')`, messageID).Scan(&recipients))
	assert.Zero(t, recipients, "unresolved display names must not create fabricated participant addresses")
	var matches int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, "uniquebodytoken").Scan(&matches))
	assert.Equal(t, 1, matches)
}
