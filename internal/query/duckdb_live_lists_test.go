package query

import (
	"context"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestDuckDBEngineLiveMessageListsIndependentOfCache(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	b := NewTestDataBuilder(t)
	b.AddSource("cached@example.test")
	sender := b.AddParticipant("cached@example.test", "example.test", "Cached")
	id := b.AddMessage(MessageOpt{Subject: "cached message"})
	b.AddFrom(id, sender, "Cached")
	dir, cleanup := b.Build()
	t.Cleanup(cleanup)
	db := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := db.DB.ExecContext(t.Context(), `
 INSERT INTO sources(id,source_type,identifier) VALUES(7,'gmail','live@example.test'),(8,'whatsapp','chat');
 INSERT INTO participants(id,email_address,domain,display_name) VALUES(11,'live@example.test','example.test','Live');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(701,7,'live-thread','email','Live'),(702,8,'chat','direct_chat','Chat');
 INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,subject,snippet,sender_id) VALUES
 (801,701,7,'live-1','email','2024-01-15 10:00:00','live subject','live snippet',11),
 (802,702,8,'chat-1','whatsapp','2024-01-15 10:00:00','chat','chat snippet',11),
 (803,701,7,'live-2','email','2024-01-15 10:00:00','second live','second snippet',11);
 INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(801,11,'from'),(802,11,'from'),(803,11,'from');
 `)
	req.NoError(err)
	enabled, err := NewDuckDBEngine(t.Context(), dir, "", db.DB, DuckDBOptions{DisableSQLiteScanner: true, LiveSQLMessageLists: true})
	req.NoError(err)
	t.Cleanup(func() { req.NoError(enabled.Close()) })
	legacy, err := NewDuckDBEngine(t.Context(), dir, "", db.DB, DuckDBOptions{DisableSQLiteScanner: true})
	req.NoError(err)
	t.Cleanup(func() { req.NoError(legacy.Close()) })
	cached, err := legacy.ListMessages(t.Context(), MessageFilter{})
	req.NoError(err)
	req.Len(cached, 1)
	check.Equal("cached message", cached[0].Subject, "explicit analytical behavior remains available")
	// Hold the actual publication lock. A basic live list must complete before
	// this lock is released, rather than waiting for an analytics reader slot.
	lock := flock.New(CacheBuildLockPath(dir))
	req.NoError(lock.Lock())
	defer func() { req.NoError(lock.Unlock()) }()
	after := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	before := after.Add(24 * time.Hour)
	for _, tc := range []struct {
		name   string
		offset int
		want   int64
	}{{"first", 0, 803}, {"second", 1, 801}} {
		t.Run(tc.name, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			ctx, cancel := context.WithTimeout(sourcescope.WithSourceIDs(t.Context(), []int64{7}), 5*time.Second)
			defer cancel()
			rows, err := enabled.ListMessages(ctx, MessageFilter{After: &after, Before: &before, Sorting: MessageSorting{Field: MessageSortByDate, Direction: SortAsc}, Pagination: Pagination{Limit: 1, Offset: tc.offset}})
			req.NoError(err, "list must proceed while cache publication remains locked")
			req.Len(rows, 1)
			check.Equal(tc.want, rows[0].ID)
			check.Equal(int64(7), rows[0].SourceID)
			check.Equal("live@example.test", rows[0].FromEmail)
		})
	}
}
