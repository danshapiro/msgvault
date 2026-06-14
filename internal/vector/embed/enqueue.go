package embed

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

// Enqueuer inserts message IDs into pending_embeddings for every
// non-retired generation. Implements the EmbedEnqueuer interface
// expected by internal/sync.
//
// Dual-enqueue is intentional: when a rebuild is in progress there are
// two non-retired generations (active + building); every newly-synced
// message gets queued into both so the building index stays current.
type Enqueuer struct {
	db     *sql.DB
	mainDB *sql.DB
	scope  vector.BuildScope
}

// NewEnqueuer returns an Enqueuer backed by vectors.db.
func NewEnqueuer(db *sql.DB) *Enqueuer {
	return NewScopedEnqueuer(db, nil, vector.BuildScope{})
}

// NewScopedEnqueuer returns an Enqueuer that only queues messages
// matching the supplied build scope. mainDB is required when scope is
// non-empty so message IDs can be checked against messages.message_type.
func NewScopedEnqueuer(db *sql.DB, mainDB *sql.DB, scope vector.BuildScope) *Enqueuer {
	return &Enqueuer{
		db:     db,
		mainDB: mainDB,
		scope:  vector.NewBuildScope(scope.MessageTypes),
	}
}

// EnqueueMessages adds the given IDs to pending_embeddings for every
// generation not in state 'retired'. Duplicate IDs are silently ignored
// via INSERT OR IGNORE. Caller must only pass non-deleted message IDs —
// the deletion predicate is not checked here.
func (e *Enqueuer) EnqueueMessages(ctx context.Context, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	filteredIDs, err := e.filterMessageIDs(ctx, messageIDs)
	if err != nil {
		return err
	}
	if len(filteredIDs) == 0 {
		return nil
	}
	messageIDs = filteredIDs

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	gens, err := func() ([]int64, error) {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM index_generations WHERE state != ?`,
			string(vector.GenerationRetired))
		if err != nil {
			return nil, fmt.Errorf("select non-retired generations: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan generation id: %w", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate generations: %w", err)
		}
		return out, nil
	}()
	if err != nil {
		return err
	}
	if len(gens) == 0 {
		return tx.Commit()
	}

	// Bulk-insert one row per (gen, message) pair via a single statement
	// per generation, expanded with json_each(message_ids). For a 5,000-
	// message incremental batch with two non-retired generations, this
	// is two writes against the vectors.db lock instead of 10,000 — keeps
	// the embed worker's Claim from starving while sync flushes.
	blob, err := json.Marshal(messageIDs)
	if err != nil {
		return fmt.Errorf("encode message ids: %w", err)
	}
	now := time.Now().Unix()
	for _, g := range gens {
		if _, err := tx.ExecContext(ctx, `
            INSERT OR IGNORE INTO pending_embeddings (generation_id, message_id, enqueued_at)
            SELECT ?, value, ? FROM json_each(?)`,
			g, now, string(blob)); err != nil {
			return fmt.Errorf("insert pending (gen=%d): %w", g, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue: %w", err)
	}
	return nil
}

func (e *Enqueuer) filterMessageIDs(ctx context.Context, messageIDs []int64) ([]int64, error) {
	if e.scope.IsEmpty() {
		return messageIDs, nil
	}
	if e.mainDB == nil {
		return nil, fmt.Errorf("main db is required for scoped embedding enqueue")
	}
	blob, err := json.Marshal(messageIDs)
	if err != nil {
		return nil, fmt.Errorf("encode message ids: %w", err)
	}
	placeholders := make([]string, len(e.scope.MessageTypes))
	args := make([]any, 0, 1+len(e.scope.MessageTypes))
	args = append(args, string(blob))
	for i, typ := range e.scope.MessageTypes {
		placeholders[i] = "?"
		args = append(args, typ)
	}
	rows, err := e.mainDB.QueryContext(ctx, fmt.Sprintf(`
		SELECT m.id
		  FROM messages m
		  JOIN json_each(?) ids ON m.id = CAST(ids.value AS INTEGER)
		 WHERE m.message_type IN (%s)`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("filter scoped message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]int64, 0, len(messageIDs))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan scoped message id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scoped message ids: %w", err)
	}
	return out, nil
}
