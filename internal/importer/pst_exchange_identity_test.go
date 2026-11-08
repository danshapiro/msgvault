package importer

import (
	"database/sql"
	"log/slog"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pstreader "go.kenn.io/msgvault/internal/pst"
	"go.kenn.io/msgvault/internal/query"
)

// Real PST ingestion should reuse identities only when the Exchange address
// can identify the same sender within a source, while retaining each message's
// original display name and never manufacturing an email address.
func TestPSTExchangeIdentityReuseAndIsolation(t *testing.T) {
	st := openIntegrationStore(t)
	sourceA, err := st.GetOrCreateSource("pst", "first@example.test")
	require.NoError(t, err)
	sourceB, err := st.GetOrCreateSource("pst", "second@example.test")
	require.NoError(t, err)
	engine := query.NewSQLiteEngine(st.DB())

	type ingestedMessage struct{ messageID, senderID int64 }
	ingest := func(sourceID int64, sourceIdentifier, messageID, name, address string, author bool) ingestedMessage {
		t.Helper()
		addressType, body := "EX", "Synthetic body"
		props := &properties.Message{Body: &body}
		if author {
			props.SentRepresentingName = &name
			props.SentRepresentingAddressType = &addressType
			props.SentRepresentingEmailAddress = &address
		} else {
			props.SenderName = &name
			props.SenderAddressType = &addressType
			props.SenderEmailAddress = &address
		}
		entry := pstreader.ExtractMessage(&pstlib.Message{Properties: props}, "Inbox")
		require.NotNil(t, entry)
		require.NoError(t, entry.HeaderError)
		raw, buildErr := pstreader.BuildRFC5322(entry, nil)
		require.NoError(t, buildErr)
		require.NoError(t, pstMessageIngester(nil)(t.Context(), st, sourceID, sourceIdentifier, "", nil,
			messageID, messageID, raw, time.Time{}, slog.Default()))
		var id, senderID int64
		var email sql.NullString
		require.NoError(t, st.DB().QueryRow(`SELECT m.id, m.sender_id, p.email_address
			FROM messages m JOIN participants p ON p.id = m.sender_id
			WHERE m.source_id = ? AND m.source_message_id = ?`, sourceID, messageID).Scan(&id, &senderID, &email))
		assert.False(t, email.Valid && email.String != "", "unresolved Exchange identity cannot become SMTP")
		detail, detailErr := engine.GetMessage(t.Context(), id)
		require.NoError(t, detailErr)
		require.Len(t, detail.From, 1)
		assert.Equal(t, name, detail.From[0].Name)
		assert.Empty(t, detail.From[0].Email)
		return ingestedMessage{id, senderID}
	}

	identity := "/O=EXAMPLE/OU=OFFICE/CN=PERSON"
	first := ingest(sourceA.ID, sourceA.Identifier, "first", "Original Name", identity, false)
	renamed := ingest(sourceA.ID, sourceA.Identifier, "renamed", "Changed Name", "/o=example/ou=office/cn=person", false)
	represented := ingest(sourceA.ID, sourceA.Identifier, "represented", "Represented Name", identity, true)
	assert.Equal(t, first.senderID, renamed.senderID, "case variants of the same Exchange identity should reuse a sender")
	assert.Equal(t, first.senderID, represented.senderID, "the same source identity should be shared across sender roles")
	for _, item := range []struct {
		message ingestedMessage
		name    string
	}{{first, "Original Name"}, {renamed, "Changed Name"}, {represented, "Represented Name"}} {
		detail, detailErr := engine.GetMessage(t.Context(), item.message.messageID)
		require.NoError(t, detailErr)
		require.Len(t, detail.From, 1)
		assert.Equal(t, item.name, detail.From[0].Name, "each message retains its own name")
	}
	otherSource := ingest(sourceB.ID, sourceB.Identifier, "first", "Original Name", identity, false)
	assert.NotEqual(t, first.senderID, otherSource.senderID, "identity must be scoped to its source")
	otherDN := ingest(sourceA.ID, sourceA.Identifier, "other", "Original Name", "/O=EXAMPLE/OU=OFFICE/CN=OTHER", false)
	assert.NotEqual(t, first.senderID, otherDN.senderID, "matching names cannot merge distinct Exchange identities")

	for _, tc := range []struct{ label, address string }{
		{"missing", ""},
		{"not distinguished name", "example address"},
		{"missing component", "/O=EXAMPLE/OU=OFFICE/CN="},
		{"ambiguous whitespace", " /O=EXAMPLE/OU=OFFICE/CN=PERSON"},
		{"control character", "/O=EXAMPLE/OU=OFFICE/CN=PERSON\r\n"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			firstID := ingest(sourceA.ID, sourceA.Identifier, tc.label+"-1", "Same Display Name", tc.address, false)
			secondID := ingest(sourceA.ID, sourceA.Identifier, tc.label+"-2", "Same Display Name", tc.address, false)
			assert.NotEqual(t, firstID.senderID, secondID.senderID, "ambiguous Exchange identities must not merge on name or malformed value")
		})
	}
}
