package importer

import (
	"log/slog"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

func TestPSTBinaryHTMLIsStoredAndSearchable(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	msg := &pstlib.Message{
		Identifier: 123,
		Properties: &properties.Message{},
		PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{{
			ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: []byte("<p>syntheticbinaryhtmlneedle</p>"),
		}}},
	}
	entry := pstreader.ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	raw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	require.NoError(t, IngestRawMessage(t.Context(), st, src.ID, "owner@example.test", "", nil,
		"pst-synthetic-123", "hash", raw, time.Time{}, slog.Default()))
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'syntheticbinaryhtmlneedle'`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestPSTRecipientRolesAreStored(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	entry := &pstreader.MessageEntry{
		TransportHeaders: "From: sender@example.test\r\nTo: alice@example.test\r\n",
		BodyText:         "Synthetic message",
		Recipients: []pstreader.MessageRecipient{
			{Email: "alice@example.test", Role: 1},
			{Email: "bob@example.test", Role: 1},
			{Email: "bob@example.test", Role: 2},
			{Email: "eve@example.test", Role: 3},
		},
	}
	raw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	require.NoError(t, IngestRawMessage(t.Context(), st, src.ID, "owner@example.test", "", nil,
		"pst-synthetic-456", "hash", raw, time.Time{}, slog.Default()))
	rows, err := st.DB().Query(`SELECT recipient_type, email_address FROM message_recipients`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var got []string
	for rows.Next() {
		var role, address string
		require.NoError(t, rows.Scan(&role, &address))
		got = append(got, role+":"+address)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, []string{"from:sender@example.test", "to:alice@example.test", "to:bob@example.test", "cc:bob@example.test", "bcc:eve@example.test"}, got)
}
