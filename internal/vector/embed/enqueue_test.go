//go:build sqlite_vec

package embed

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestEnqueuer_NoGenerations_Noop(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	e := NewEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{1, 2, 3}), "EnqueueMessages with no generations")
	// Should be no pending rows.
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_embeddings`).Scan(&n))
	assert.Equal(t, 0, n, "pending count")
}

func TestEnqueuer_ActiveGenerationOnly(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")
	e := NewEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{10, 11}))
	assertPending(t, db, 1, 2)
}

func TestEnqueuer_ActiveAndBuilding_DualEnqueue(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")
	insertGenerationStatic(t, db, 2, "building")
	insertGenerationStatic(t, db, 3, "retired") // should NOT receive.
	e := NewEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{100}))
	assertPending(t, db, 1, 1)
	assertPending(t, db, 2, 1)
	assertPending(t, db, 3, 0)
}

func TestEnqueuer_DuplicateIDs_Ignored(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")
	e := NewEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{42}))
	// Second call with same ID should not error; count still 1.
	require.NoError(t, e.EnqueueMessages(ctx, []int64{42, 42}))
	assertPending(t, db, 1, 1)
}

func TestEnqueuer_EmptyIDs_Noop(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")
	e := NewEnqueuer(db)
	assert.NoError(t, e.EnqueueMessages(ctx, nil), "EnqueueMessages(nil)")
	assert.NoError(t, e.EnqueueMessages(ctx, []int64{}), "EnqueueMessages([])")
	assertPending(t, db, 1, 0)
}

func TestEnqueuer_ScopedMessageTypes(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")

	mainDB, err := sql.Open(sqlitevec.DriverName(), ":memory:")
	require.NoError(t, err, "open main")
	t.Cleanup(func() { _ = mainDB.Close() })
	_, err = mainDB.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		message_type TEXT NOT NULL
	)`)
	require.NoError(t, err, "create messages")
	_, err = mainDB.Exec(`
		INSERT INTO messages (id, message_type) VALUES
		(10, 'sms'),
		(11, 'email'),
		(12, 'mms')`)
	require.NoError(t, err, "insert messages")

	e := NewScopedEnqueuer(db, mainDB, vector.NewBuildScope([]string{"sms", "mms"}))
	require.NoError(t, e.EnqueueMessages(ctx, []int64{10, 11, 12}))

	assertPending(t, db, 1, 2)
	var queuedEmail int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = 1 AND message_id = 11`).Scan(&queuedEmail))
	assert.Equal(t, 0, queuedEmail)
}
