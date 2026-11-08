package importer

import (
	"log/slog"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
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
