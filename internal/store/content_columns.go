package store

import (
	"fmt"
	"strings"
)

// MessagesContentColumns are the columns of `messages` whose modification means
// the message changed in a way a reader must see again. This list drives the
// content_changed_at triggers on both backends.
//
// The invariant tying this list to the change feed is one-directional: every
// field the feed returns must appear here, because a field the endpoint reports
// but the trigger ignores would be cached stale by a consumer forever. The
// converse does not hold. This list also covers columns the feed does not
// return — sender_id and metadata are tracked because changing either means
// "re-read this message", yet neither is in the response — and that is correct:
// tracking a column the feed omits costs a redundant re-read, while omitting a
// column the feed returns costs silent staleness.
// TestChangesResponseFieldsAreAllTracked asserts the direction that matters;
// TestMessagesColumnClassificationIsExhaustive asserts every real column is
// classified.
//
// last_modified is NOT this list. That column is a true row-level watermark
// that bumps on any change and stays that way -- the embed worker relies on it
// as an optimistic-CAS token. The two columns answer different questions.
//
// Adding a column to `messages` without classifying it here or in
// MessagesNonContentColumns fails TestMessagesColumnClassificationIsExhaustive.
var MessagesContentColumns = []string{
	// source_message_id is NOT immutable, despite reading like a natural key:
	// UpdateMessageOnDedup (messages.go:447) rewrites it on a cross-mailbox
	// RFC822 dedup match and MigrateSourceMessageID (messages.go:465) rewrites
	// it when a message moves between source locations. The feed returns it, so
	// leaving it untracked would strand a consumer on a stale source ID.
	"source_message_id",
	"conversation_id",        // message moved threads
	"sender_id",              // sender re-resolved or corrected
	"message_type",           // reclassified
	"sent_at",                // canonical timestamp corrected
	"received_at",            // platform timestamp corrected
	"internal_date",          // feeds the reported sent_at via COALESCE
	"subject",                // content
	"snippet",                // content
	"metadata",               // platform-specific payload
	"size_estimate",          // reported to consumers
	"has_attachments",        // attachment set changed
	"attachment_count",       // attachment set changed
	"deleted_at",             // dedup-hidden / restored
	"deleted_from_source_at", // removed at the source
}

// MessagesNonContentColumns are the remaining columns of `messages`. Changing
// one does not move content_changed_at. Each carries its reason: the cost of a
// wrong call here is a consumer that silently misses updates.
var MessagesNonContentColumns = []string{
	"id",                  // immutable identity
	"source_id",           // immutable: which account this came from
	"rfc822_message_id",   // not reported by the feed (dedup.go rewrites it)
	"read_at",             // local read state, not archive content
	"delivered_at",        // platform delivery receipt
	"is_from_me",          // not reported by the feed
	"reply_to_message_id", // threading pointer; conversation_id is the routing key
	"thread_position",     // ordering within a thread, derived
	"is_read",             // local read state
	"is_delivered",        // platform delivery state
	"is_sent",             // platform send state
	"is_edited",           // flag; the edit itself lands in subject/snippet/body
	"is_forwarded",        // platform flag
	"delete_batch_id",     // deletion-run bookkeeping; deleted_at carries the fact
	"archived_at",         // when we archived it, not when it changed
	"indexing_version",    // FTS index bookkeeping
	"last_modified",       // the other watermark
	"content_changed_at",  // itself
	"embed_gen",           // embedding watermark -- the whole reason this list exists
	"search_fts",          // PostgreSQL-only tsvector, maintained by the FTS path
}

// ContentChangedTriggerColumnList renders MessagesContentColumns for a
// `... UPDATE OF <cols> ON messages ...` clause. Both dialects call this, so
// their trigger definitions cannot disagree.
func ContentChangedTriggerColumnList() string {
	return strings.Join(MessagesContentColumns, ", ")
}

// ContentChangedValueGuard renders the "did any content column actually change
// value?" half of the trigger's WHEN clause.
//
// The column list alone is not enough. Both backends fire `UPDATE OF` on the
// columns a statement NAMES, regardless of whether the value changed (measured
// on both), and UpsertMessage's `ON CONFLICT ... DO UPDATE SET`
// (messages.go:581-606) unconditionally re-assigns ten content columns on
// every re-sync of a known message. Without this guard every message a sync
// touches reports as changed and the feed carries no information.
//
// The comparison is null-safe both ways, so NULL->value and value->NULL count
// as changes and NULL->NULL does not. This mirrors the idiom the upsert already
// uses at messages.go:592 to decide whether a subject really changed.
//
// distinctOp is "IS NOT" for SQLite, "IS DISTINCT FROM" for PostgreSQL.
func ContentChangedValueGuard(distinctOp string) string {
	clauses := make([]string, 0, len(MessagesContentColumns))
	for _, c := range MessagesContentColumns {
		clauses = append(clauses, fmt.Sprintf("OLD.%s %s NEW.%s", c, distinctOp, c))
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

// MessagesTableColumns returns the live column names of the messages table on
// whichever backend the store uses. Kept beside the lists it guards.
func MessagesTableColumns(s *Store) ([]string, error) {
	q := `SELECT name FROM pragma_table_info('messages')`
	if s.IsPostgreSQL() {
		q = `SELECT column_name FROM information_schema.columns
		     WHERE table_name = 'messages' AND table_schema = current_schema()
		     ORDER BY ordinal_position`
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("read messages columns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan messages column: %w", err)
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}
