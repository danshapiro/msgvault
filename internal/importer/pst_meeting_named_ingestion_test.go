package importer

import (
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	msgmime "go.kenn.io/msgvault/internal/mime"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

// The real extraction and archive path must retain unknown and binary named
// values even when they cannot yet be interpreted as a calendar event.
func TestPSTMeetingNamedPropertiesSurviveArchiveIngestion(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	start := time.Date(2024, 3, 4, 15, 30, 0, 0, time.UTC)
	startBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(startBytes, uint64(start.Unix()+11644473600)*10000000)
	namedMap := &pstlib.NameToIDMap{NameToID: map[int]int{
		int(pstlib.PropertySetAppointment)<<32 | 0x820d: 0x8001,
		int(pstlib.PropertySetAppointment)<<32 | 0x8208: 0x8002,
		int(pstlib.PropertySetAppointment)<<32 | 0x8232: 0x8003,
	}}
	sources := []pstlib.Property{
		{ID: 0x8001, Type: pstlib.PropertyTypeTime, Data: startBytes},
		{ID: 0x8002, Type: pstlib.PropertyTypeString8, Data: []byte("Room A\x00")},
		{ID: 0x8003, Type: pstlib.PropertyTypeString8, Data: []byte("Every Tuesday\x00")},
		{ID: 0x8004, Type: pstlib.PropertyTypeBinary, Data: []byte{0x04, 0x00, 0xff, 0x7d, 0x03}},             // recurrence payload
		{ID: 0x8005, Type: pstlib.PropertyTypeBinary, Data: []byte{0x00, 0x01, 0x02, 0xfe, 0xff}},             // time-zone payload
		{ID: 0x8006, Type: pstlib.PropertyTypeBinary, Data: []byte{0x04, 0x00, 0x00, 0x00, 0x82, 0x00, 0xe0}}, // meeting identity payload
	}
	props := append([]pstlib.Property{
		{ID: 0x001a, Type: pstlib.PropertyTypeString8, Data: []byte("IPM.Schedule.Meeting.Request\x00")},
		{ID: 0x1000, Type: pstlib.PropertyTypeString8, Data: []byte("Synthetic meeting body\x00")},
	}, sources...)
	msg := &pstlib.Message{Identifier: 321, Properties: &properties.Appointment{}, File: &pstlib.File{NameToIDMap: namedMap}, PropertyContext: &pstlib.PropertyContext{Properties: props}}
	entry := pstreader.ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	require.NoError(t, entry.BodyError)
	require.NotNil(t, entry.CalendarAttachment)
	raw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	ingest := pstMessageIngester(nil)
	for range 2 {
		require.NoError(t, ingest(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-synthetic-321", "hash", raw, time.Time{}, slog.Default()))
	}
	var id, count int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-synthetic-321").Scan(&id))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-synthetic-321").Scan(&count))
	assert.Equal(t, int64(1), count)
	stored, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	parsed, err := msgmime.Parse(stored)
	require.NoError(t, err)
	assert.Contains(t, parsed.BodyText, "Synthetic meeting body")
	require.Len(t, parsed.Attachments, 1)
	assert.Equal(t, "pst-meeting-properties.json", parsed.Attachments[0].Filename)
	var facts struct {
		Version    int    `json:"version"`
		Start      string `json:"start_utc"`
		Location   string `json:"location"`
		Pattern    string `json:"recurrence_pattern"`
		Properties []struct {
			ID      uint16 `json:"property_id"`
			Type    uint16 `json:"property_type"`
			Storage string `json:"storage"`
			Value   []byte `json:"value_base64"`
		} `json:"named_properties"`
	}
	require.NoError(t, json.Unmarshal(parsed.Attachments[0].Content, &facts))
	assert.Equal(t, 1, facts.Version)
	assert.Equal(t, start.Format(time.RFC3339), facts.Start)
	assert.Equal(t, "Room A", facts.Location)
	assert.Equal(t, "Every Tuesday", facts.Pattern)
	require.Len(t, facts.Properties, len(sources))
	for i, want := range sources {
		assert.Equal(t, want.ID, facts.Properties[i].ID)
		assert.Equal(t, uint16(want.Type), facts.Properties[i].Type)
		assert.Equal(t, "inline", facts.Properties[i].Storage)
		assert.Equal(t, want.Data, facts.Properties[i].Value)
	}
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	archived, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
	require.NoError(t, err)
	assert.Equal(t, parsed.Attachments[0].Content, archived)
}
