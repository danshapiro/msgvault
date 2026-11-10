//go:build transition_smoke

package store_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// httpReadFixtureHome accepts only an explicitly selected disposable directory
// in this worktree. A missing opt-in is a failure in this opt-in build profile.
func httpReadFixtureHome(t *testing.T) string {
	t.Helper()
	home := os.Getenv("MSGVAULT_SMOKE_HOME")
	require.NotEmpty(t, home, "set MSGVAULT_SMOKE_HOME inside this worktree's .test-tmp")
	require.True(t, filepath.IsAbs(home))
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	parent, err := filepath.EvalSymlinks(filepath.Dir(home))
	require.NoError(t, err)
	home = filepath.Join(parent, filepath.Base(home))
	relative, err := filepath.Rel(filepath.Join(root, ".test-tmp"), home)
	require.NoError(t, err)
	require.NotEqual(t, ".", relative)
	require.False(t, relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)),
		"fixture must stay in this worktree's .test-tmp")
	return home
}

// TestCreateHTTPReadFixture creates a schema-complete, synthetic archive for
// independently built HTTP consumers. It never connects to a provider.
func TestCreateHTTPReadFixture(t *testing.T) {
	home := httpReadFixtureHome(t)
	require.NoError(t, os.Mkdir(home, 0700), "refuse an existing fixture directory or archive")
	s, err := store.OpenForTest(t.Context(), filepath.Join(home, "msgvault.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	require.NoError(t, s.InitSchema())

	source, err := s.GetOrCreateSourceContext(t.Context(), "gmail", "archive@example.test")
	require.NoError(t, err)
	conversation, err := s.EnsureConversation(t.Context(), source.ID, "synthetic-mail", "Synthetic mail")
	require.NoError(t, err)
	calendar, err := s.EnsureConversation(t.Context(), source.ID, "synthetic-calendar", "Synthetic calendar")
	require.NoError(t, err)
	participants := make(map[string]int64)
	for _, address := range []string{"sender@example.test", "recipient@example.test", "calendar@example.test",
		"review@example.test", "cc@example.test", "bcc@example.test"} {
		id, err := s.EnsureParticipantContext(t.Context(), address, "", "example.test")
		require.NoError(t, err)
		participants[address] = id
	}
	sent, err := s.EnsureLabel(t.Context(), source.ID, "SENT", "SENT", "system")
	require.NoError(t, err)
	synthetic, err := s.EnsureLabel(t.Context(), source.ID, "synthetic", "Synthetic", "user")
	require.NoError(t, err)
	write := func(wantID int64, kind string, date time.Time, from, to, cc, bcc string, sentLabel bool) {
		t.Helper()
		conv := conversation
		if kind == "calendar_event" {
			conv = calendar
		}
		data := &store.MessagePersistData{
			Message: &store.Message{SourceID: source.ID, ConversationID: conv,
				SourceMessageID: fmt.Sprintf("synthetic-%d", wantID), MessageType: kind,
				SentAt:  sql.NullTime{Time: date, Valid: true},
				Subject: sql.NullString{String: fmt.Sprintf("Synthetic message %d", wantID), Valid: true},
				Snippet: sql.NullString{String: "Synthetic HTTP fixture", Valid: true},
			},
			LabelIDs: []int64{synthetic},
			BodyText: sql.NullString{String: "Synthetic body for an HTTP read integration check.", Valid: true},
		}
		if sentLabel {
			data.LabelIDs = append(data.LabelIDs, sent)
		}
		for i, address := range []string{from, to, cc, bcc} {
			if address == "" {
				continue
			}
			data.Recipients = append(data.Recipients, store.RecipientSet{
				Type:           []string{"from", "to", "cc", "bcc"}[i],
				ParticipantIDs: []int64{participants[strings.ToLower(address)]},
				EmailAddresses: []string{address},
			})
		}
		id, err := s.PersistMessageContext(t.Context(), data)
		require.NoError(t, err)
		require.Equal(t, wantID, id)
	}
	for id := int64(1); id <= 350; id++ {
		write(id, "calendar_event", time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id)*time.Hour),
			"calendar@example.test", "", "", "", false)
	}
	current := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	old := time.Date(2016, 1, 2, 12, 0, 0, 0, time.UTC)
	write(351, "email", current, "sender@example.test", "recipient@example.test", "cc@example.test", "bcc@example.test", true)
	write(352, "email", old, "sender@example.test", "recipient@example.test", "cc@example.test", "bcc@example.test", true)
	write(353, "email", old, "sender@example.test", "", "", "", false)
	require.NoError(t, s.MarkMessageDeletedContext(t.Context(), source.ID, "synthetic-353"))
	write(354, "email", old, "sender@example.test", "", "", "", false)
	_, err = s.MergeDuplicates(t.Context(), 351, []int64{354}, "synthetic-smoke")
	require.NoError(t, err)
	write(355, "email", old, "sender@example.test", "review@example.test", "", "", true)
	write(356, "email", current, "sender@example.test", "REVIEW@EXAMPLE.TEST", "", "", true)
	write(357, "email", old, "sender@example.test", "", "review@example.test", "", true)
	write(358, "email", old, "sender@example.test", "", "", "review@example.test", true)
	write(359, "email", old, "sender@example.test", "", "recipient@example.test", "", false)

	// Synthetic feed timestamps are intentionally tied across every page. Email
	// metadata and recipients above use the same Store writers as imports; only
	// this test clock is set explicitly through the Store's supported DB handle.
	_, err = s.DB().ExecContext(t.Context(),
		"UPDATE messages SET content_changed_at = ?", "2026-01-01 00:00:00.000")
	require.NoError(t, err)
	var cursor store.ChangedMessagesCursor
	var changes []store.ChangedMessage
	for {
		page, err := s.ListChangedMessages(t.Context(), cursor, 50)
		require.NoError(t, err)
		changes = append(changes, page.Messages...)
		if len(page.Messages) < 50 {
			break
		}
		last := page.Messages[len(page.Messages)-1]
		cursor = store.ChangedMessagesAfter(last.ContentChangedAt, last.ID)
	}
	require.Len(t, changes, 359)
	for i, change := range changes {
		assert.Equal(t, int64(i+1), change.ID)
		assert.Equal(t, changes[0].ContentChangedAt, change.ContentChangedAt)
	}
	assert.NotNil(t, changes[352].DeletedFromSourceAt)
	assert.NotNil(t, changes[353].DeletedAt)
	messages, total, err := s.ListMessagesContext(t.Context(), 0, 100, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(357), total)
	require.Len(t, messages, 100)
	assert.Equal(t, int64(350), messages[0].ID)
	for _, id := range []int64{351, 352, 355, 356} {
		message, err := s.GetMessageContext(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, "sender@example.test", message.From)
		assert.Len(t, message.To, 1)
		assert.Contains(t, message.Labels, "SENT")
	}
	recipients, err := s.GetMessageRecipientsContext(t.Context(), 356, "to")
	require.NoError(t, err)
	require.Len(t, recipients, 1)
	assert.Equal(t, "REVIEW@EXAMPLE.TEST", recipients[0].EnvelopeAddress)

	text := fmt.Sprintf(`[data]
data_dir = %q
[server]
bind_addr = "127.0.0.1"
api_port = 18880
api_key = "transition-smoke-only"
daemon_auto_start = false
[telemetry]
enabled = false
[activity]
schedule = ""
`, home)
	configPath := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(text), 0600))
	snapshot, err := config.ReadConfigFile(configPath)
	require.NoError(t, err)
	cfg, err := config.LoadConfigFile(snapshot, home)
	require.NoError(t, err)
	assert.Equal(t, home, cfg.Data.DataDir)
	assert.Equal(t, filepath.Join(home, "msgvault.db"), cfg.DatabaseDSN())
	assert.Empty(t, cfg.Accounts)
	assert.False(t, cfg.Server.DaemonAutoStartEnabled())

	manifest := map[string]any{
		"source_id": source.ID, "target_ids": []int64{351, 352},
		"target_sent_at": []time.Time{current, old}, "source_deleted_id": 353, "internally_deleted_id": 354,
		"review_ids": []int64{355, 356}, "cc_only_id": 357, "bcc_only_id": 358, "metadata_id": 359,
		"sender": "sender@example.test", "recipient": "recipient@example.test", "review_recipient": "review@example.test",
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "fixture.json"), encoded, 0600))
	t.Logf("created and verified synthetic archive: %s (359 changes, 357 live; targets 351/352)", home)
}

// TestUpdateHTTPReadFixtureMetadata is run only after stopping the test-owned
// daemon. It adds To/SENT through supported Store writers, without changing any
// message content or its change-feed watermark.
func TestUpdateHTTPReadFixtureMetadata(t *testing.T) {
	home := httpReadFixtureHome(t)
	data, err := os.ReadFile(filepath.Join(home, "fixture.json"))
	require.NoError(t, err)
	var manifest struct {
		SourceID   int64 `json:"source_id"`
		MetadataID int64 `json:"metadata_id"`
	}
	require.NoError(t, json.Unmarshal(data, &manifest))
	require.Equal(t, int64(359), manifest.MetadataID)
	s, err := store.OpenForTest(t.Context(), filepath.Join(home, "msgvault.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	before, err := s.ListChangedMessages(t.Context(), store.ChangedMessagesCursor{}, 500)
	require.NoError(t, err)
	require.Len(t, before.Messages, 359)
	message, err := s.GetMessageContext(t.Context(), manifest.MetadataID)
	require.NoError(t, err)
	require.Equal(t, "synthetic-359", message.SourceMessageID)
	recipient, err := s.EnsureParticipantContext(t.Context(), "recipient@example.test", "", "example.test")
	require.NoError(t, err)
	require.NoError(t, s.ReplaceMessageRecipientsContext(t.Context(), manifest.MetadataID, "to", []int64{recipient}, nil))
	sent, err := s.EnsureLabel(t.Context(), manifest.SourceID, "SENT", "SENT", "system")
	require.NoError(t, err)
	require.NoError(t, s.AddMessageLabels(t.Context(), manifest.MetadataID, []int64{sent}))
	after, err := s.ListChangedMessages(t.Context(), store.ChangedMessagesCursor{}, 500)
	require.NoError(t, err)
	assert.Equal(t, before.Messages, after.Messages, "metadata-only changes stay absent from the content feed")
	message, err = s.GetMessageContext(t.Context(), manifest.MetadataID)
	require.NoError(t, err)
	assert.Equal(t, []string{"recipient@example.test"}, message.To)
	assert.Contains(t, message.Labels, "SENT")
}
