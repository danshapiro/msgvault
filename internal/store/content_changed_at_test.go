package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestMessagesColumnClassificationIsExhaustive makes the hand-written trigger
// column list safe to maintain. A column added to messages later that nobody
// classifies would silently never bump the watermark, and a consumer would miss
// the change forever -- invisible in production and untestable after the fact.
// Every real column must appear in exactly one list.
func TestMessagesColumnClassificationIsExhaustive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	actual, err := store.MessagesTableColumns(st)
	require.NoError(err)
	require.NotEmpty(actual)

	classified := map[string]int{}
	for _, c := range store.MessagesContentColumns {
		classified[c]++
	}
	for _, c := range store.MessagesNonContentColumns {
		classified[c]++
	}

	for _, col := range actual {
		assert.Equal(1, classified[col],
			"messages.%s must appear in exactly one of MessagesContentColumns / "+
				"MessagesNonContentColumns (found %d). Classify it: does changing it "+
				"mean a consumer should re-read the message?", col, classified[col])
	}

	actualSet := map[string]bool{}
	for _, col := range actual {
		actualSet[col] = true
	}
	for col := range classified {
		if col == "search_fts" && !st.IsPostgreSQL() {
			continue // PostgreSQL-only column
		}
		assert.True(actualSet[col], "%q is classified but is not a column of messages", col)
	}
}

// contentChangedPast is the fixed far-past watermark the helpers stamp before
// exercising a write, so "did the trigger fire?" is an exact string comparison
// instead of a sleep long enough for the clock to tick. It is written in a form
// both backends accept: SQLite stores the text verbatim in its DATETIME column,
// PostgreSQL parses it as TIMESTAMPTZ.
const contentChangedPast = "2000-01-01 00:00:00+00"

// seedMessage inserts one message with a unique source_message_id derived from
// n, and NO body row, so body-insert tests have something to insert. Returns
// the message id. It deliberately does not reuse seedMessageForLM, which
// hard-codes one source_message_id (so a second call upserts the same row) and
// already inserts a body row (so a later INSERT INTO message_bodies would
// violate the primary key).
func seedMessage(t *testing.T, st *store.Store, n int) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(src.ID, fmt.Sprintf("conv-%d", n), "email_thread", "Subject")
	require.NoError(t, err, "EnsureConversationWithType")
	id, err := st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		SourceMessageID: fmt.Sprintf("msg-%d", n),
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: fmt.Sprintf("subject %d", n), Valid: true},
	})
	require.NoError(t, err, "UpsertMessage")
	return id
}

// persistMessage runs the REAL production persist path for message n:
// PersistMessage, which upserts the message row and its body row in one
// transaction exactly as every importer does. The resync tests must go through
// this rather than a hand-written UPDATE, because upsertMessageBody always
// executes its ON CONFLICT DO UPDATE even when messageBodyChanges reports no
// change -- an UpsertMessage-only test would pass while production churned on
// every sync.
func persistMessage(t *testing.T, st *store.Store, n int, subject, body string) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(src.ID, fmt.Sprintf("conv-%d", n), "email_thread", "Subject")
	require.NoError(t, err, "EnsureConversationWithType")
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("msg-%d", n),
			ConversationID:  convID,
			MessageType:     "email",
			Subject:         sql.NullString{String: subject, Valid: true},
		},
		BodyText: sql.NullString{String: body, Valid: true},
	})
	require.NoError(t, err, "PersistMessage")
	return id
}

// readContentChangedAt reads the watermark via CAST(... AS TEXT) to defeat
// go-sqlite3's DATETIME -> time.Time coercion, exactly as readLM does, and
// fails loudly on NULL: a NULL watermark is the specific defect the INSERT
// trigger exists to prevent, and it would otherwise surface as an opaque scan
// error.
func readContentChangedAt(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	got := readRawContentChangedAt(t, st, id)
	require.Truef(t, got.Valid, "content_changed_at is NULL for message %d", id)
	return got.String
}

// readRawContentChangedAt is readContentChangedAt without the non-NULL
// requirement, for the one test whose subject IS whether the value is NULL.
func readRawContentChangedAt(t *testing.T, st *store.Store, id int64) sql.NullString {
	t.Helper()
	var got sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), id).Scan(&got),
		"read content_changed_at")
	return got
}

// stampContentChangedAt writes contentChangedPast so a subsequent trigger bump
// is a different, easily-asserted value without sleeping for the clock to tick.
// The explicit write survives: the statement names only content_changed_at, so
// the UPDATE OF column list never matches and the trigger does not fire.
func stampContentChangedAt(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET content_changed_at = ? WHERE id = ?`), contentChangedPast, id)
	require.NoError(t, err, "stamp content_changed_at")
	return readContentChangedAt(t, st, id)
}

// altConversationID creates a second conversation in the same source as the
// given message and returns its id, so a conversation_id write moves the
// message to a real thread instead of tripping the foreign key.
func altConversationID(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	var sourceID int64
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT source_id FROM messages WHERE id = ?`), id).Scan(&sourceID),
		"read source_id")
	convID, err := st.EnsureConversationWithType(
		sourceID, fmt.Sprintf("conv-alt-%d", id), "email_thread", "Other thread")
	require.NoError(t, err, "EnsureConversationWithType(alt)")
	return convID
}

// altParticipantID creates a participant and returns its id, so a sender_id
// write points at a real row rather than an arbitrary integer the foreign key
// would reject.
func altParticipantID(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	pid, err := st.EnsureParticipant(
		fmt.Sprintf("sender-%d@example.com", id), "Changed Sender", "example.com")
	require.NoError(t, err, "EnsureParticipant")
	return pid
}

// updateMessageColumn writes a genuinely different, type-appropriate value to
// col. Foreign keys (conversation_id, sender_id) get a freshly created valid
// parent row rather than an arbitrary integer; metadata routes through
// SetMessageMetadata so the JSONB cast PostgreSQL requires comes from the
// dialect instead of being duplicated here.
func updateMessageColumn(t *testing.T, st *store.Store, id int64, col string) error {
	t.Helper()
	corrected := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	var value any
	switch col {
	case "source_message_id":
		value = fmt.Sprintf("changed-src-%d", id)
	case "conversation_id":
		value = altConversationID(t, st, id)
	case "sender_id":
		value = altParticipantID(t, st, id)
	case "message_type":
		value = "sms"
	case "sent_at", "received_at", "internal_date", "deleted_at", "deleted_from_source_at":
		value = corrected
	case "subject":
		value = "changed subject"
	case "snippet":
		value = "changed snippet"
	case "metadata":
		return st.SetMessageMetadata(id, sql.NullString{String: `{"changed":true}`, Valid: true})
	case "size_estimate":
		value = int64(4242)
	case "has_attachments":
		value = true
	case "attachment_count":
		value = 7
	default:
		require.Failf(t, "unhandled content column",
			"updateMessageColumn: no write defined for messages.%s -- add one so the "+
				"column's trigger coverage is actually exercised", col)
		return nil
	}
	_, err := st.DB().Exec(
		st.Rebind(fmt.Sprintf(`UPDATE messages SET %s = ? WHERE id = ?`, col)), value, id)
	return err
}

// TestContentChangedAt_ContentColumnUpdateBumps walks every column classified as
// content and proves changing it moves the watermark. Table-driven off the same
// list the triggers are built from, so a column added to the list but missing
// from the trigger fails here.
func TestContentChangedAt_ContentColumnUpdateBumps(t *testing.T) {
	st := testutil.NewTestStore(t)
	for i, col := range store.MessagesContentColumns {
		t.Run(col, func(t *testing.T) {
			id := seedMessage(t, st, i+1)
			base := stampContentChangedAt(t, st, id)
			require.NoErrorf(t, updateMessageColumn(t, st, id, col), "update messages.%s", col)
			assert.NotEqualf(t, base, readContentChangedAt(t, st, id),
				"changing messages.%s must bump content_changed_at: it is classified as "+
					"content, so a consumer that missed it would hold a stale copy forever", col)
		})
	}
}

// TestContentChangedAt_BookkeepingUpdateDoesNotBump is the other half. embed_gen
// is the motivating case: the embed worker stamps it continuously, and a
// consumer woken by every embedding stamp would re-read the archive on every
// index-generation rollover.
func TestContentChangedAt_BookkeepingUpdateDoesNotBump(t *testing.T) {
	st := testutil.NewTestStore(t)
	cases := []struct {
		col   string
		value any
	}{
		{"embed_gen", int64(7)},
		{"indexing_version", 2},
		{"is_read", false},
		{"read_at", time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)},
		{"archived_at", time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)},
		{"delete_batch_id", "batch-1"},
	}
	for i, tc := range cases {
		t.Run(tc.col, func(t *testing.T) {
			id := seedMessage(t, st, i+1)
			base := stampContentChangedAt(t, st, id)
			_, err := st.DB().Exec(
				st.Rebind(fmt.Sprintf(`UPDATE messages SET %s = ? WHERE id = ?`, tc.col)), tc.value, id)
			require.NoErrorf(t, err, "update messages.%s", tc.col)
			assert.Equalf(t, base, readContentChangedAt(t, st, id),
				"messages.%s is bookkeeping, not content: writing it must leave "+
					"content_changed_at alone", tc.col)
		})
	}
}

// TestContentChangedAt_SameValueWriteDoesNotBump: both backends fire UPDATE OF
// on the columns a statement NAMES, not the ones whose value changed. Without
// the value guard this passes vacuously and the feed reports every message a
// sync touches.
func TestContentChangedAt_SameValueWriteDoesNotBump(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)
	base := stampContentChangedAt(t, st, id)

	// Self-assignment puts five content columns in the SET list -- all that
	// UPDATE OF inspects on either backend -- while changing no value, NULL
	// columns included.
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE messages
		   SET subject = subject,
		       snippet = snippet,
		       size_estimate = size_estimate,
		       has_attachments = has_attachments,
		       conversation_id = conversation_id
		 WHERE id = ?`), id)
	require.NoError(t, err, "same-value update")

	assert.Equal(t, base, readContentChangedAt(t, st, id),
		"an UPDATE naming content columns but changing no value must not bump "+
			"content_changed_at; the UPDATE OF column list alone is not enough")
}

// TestContentChangedAt_ResyncOfUnchangedMessageDoesNotBump exercises the real
// production path, not a hand-written UPDATE: PersistMessage on a message the
// archive already holds, unchanged, must leave the watermark alone. It must go
// through PersistMessage (message upsert + body upsert in one transaction),
// because upsertMessageBody always executes its ON CONFLICT DO UPDATE even when
// messageBodyChanges reports no change.
func TestContentChangedAt_ResyncOfUnchangedMessageDoesNotBump(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := persistMessage(t, st, 1, "original subject", "original body")
	base := stampContentChangedAt(t, st, id)

	require.Equal(t, id, persistMessage(t, st, 1, "original subject", "original body"),
		"resync must land on the same row")

	assert.Equal(t, base, readContentChangedAt(t, st, id),
		"re-persisting an unchanged message must not bump content_changed_at: "+
			"UpsertMessage re-assigns ten content columns on every sync, and the body "+
			"upsert always runs, so an unguarded trigger reports the whole archive as changed")
}

// TestContentChangedAt_ResyncOfChangedMessageBumps is its complement, for both a
// changed subject and a changed body.
func TestContentChangedAt_ResyncOfChangedMessageBumps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	subjID := persistMessage(t, st, 1, "original subject", "shared body")
	subjBase := stampContentChangedAt(t, st, subjID)
	require.Equal(subjID, persistMessage(t, st, 1, "corrected subject", "shared body"),
		"subject resync must land on the same row")
	assert.NotEqual(subjBase, readContentChangedAt(t, st, subjID),
		"a resync carrying a changed subject must bump content_changed_at")

	bodyID := persistMessage(t, st, 2, "steady subject", "original body")
	bodyBase := stampContentChangedAt(t, st, bodyID)
	require.Equal(bodyID, persistMessage(t, st, 2, "steady subject", "corrected body"),
		"body resync must land on the same row")
	assert.NotEqual(bodyBase, readContentChangedAt(t, st, bodyID),
		"a resync carrying a changed body must bump content_changed_at")
}

// TestContentChangedAt_BodyWriteBumpsParent: a body edit must reach the parent's
// watermark. The body lives in a separate table, so without this a
// repair-encoding pass would be invisible to a consumer.
func TestContentChangedAt_BodyWriteBumpsParent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)

	insBase := stampContentChangedAt(t, st, id)
	require.NoError(st.UpsertMessageBody(id,
		sql.NullString{String: "first body", Valid: true},
		sql.NullString{}), "insert body")
	assert.NotEqual(insBase, readContentChangedAt(t, st, id),
		"a message_bodies INSERT must bump the parent's content_changed_at")

	updBase := stampContentChangedAt(t, st, id)
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`),
		"corrected body", id)
	require.NoError(err, "update body")
	assert.NotEqual(updBase, readContentChangedAt(t, st, id),
		"a message_bodies UPDATE must bump the parent's content_changed_at")
}

// TestContentChangedAt_NewRowIsStamped proves every new message is stamped
// non-NULL, whichever writer this backend uses for inserts: the BEFORE INSERT
// trigger on PostgreSQL, the column DEFAULT on a SQLite database created from
// schema.sql (which then gets no INSERT trigger at all), the INSERT trigger on
// a SQLite database upgraded by ALTER TABLE. A NULL watermark drops the row out
// of the range query permanently.
func TestContentChangedAt_NewRowIsStamped(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)

	var nulls int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE id = ? AND content_changed_at IS NULL`),
		id).Scan(&nulls), "count NULL content_changed_at")

	assert.Equal(t, 0, nulls,
		"every new message must be stamped by whichever writer this backend uses for "+
			"inserts, since a NULL watermark would hide the row from the feed forever")
	assert.NotEqual(t, contentChangedPast, readContentChangedAt(t, st, id),
		"a freshly inserted message must be stamped with the current time")
}

// TestContentChangedAt_LastModifiedUnaffected is the compatibility proof: a
// bookkeeping-only UPDATE still bumps last_modified, which the embed worker's
// CAS depends on. If this fails, the change has stopped being additive.
func TestContentChangedAt_LastModifiedUnaffected(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)

	// Order matters: stamping content_changed_at is itself an UPDATE, so it
	// bumps last_modified. Baseline last_modified after it, not before.
	ccBase := stampContentChangedAt(t, st, id)
	lmBase := baselineLM(t, st, id)

	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET embed_gen = ? WHERE id = ?`), int64(7), id)
	require.NoError(err, "bookkeeping-only update")

	assert.NotEqual(lmBase, readLM(t, st, id),
		"last_modified must still bump on ANY update: it is the embed worker's CAS token")
	assert.Equal(ccBase, readContentChangedAt(t, st, id),
		"the same bookkeeping-only update must leave content_changed_at alone")
}

// stampLastModified writes an explicit last_modified value directly. It names
// only last_modified, so no UPDATE OF list matches and the write is not
// re-bumped by a trigger. The upgrade test needs a distinguishable, known-past
// value to prove the content_changed_at backfill seeds from last_modified
// rather than from "now"; unlike stampContentChangedAt (which stamps
// content_changed_at to a fixed constant), the value here must vary and must
// survive SQLite's strftime parsing in the backfill SQL, so it is passed with
// no timezone suffix -- a bare "YYYY-MM-DD HH:MM:SS" is the one form both
// SQLite's strftime and PostgreSQL's TIMESTAMPTZ parser accept unambiguously.
func stampLastModified(t *testing.T, st *store.Store, id int64, value string) {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET last_modified = ? WHERE id = ?`), value, id)
	require.NoError(t, err, "stamp last_modified")
}

// contentChangedAtTriggerNames are the four triggers EnsureTriggers can create
// (dialect_sqlite.go, dialect_pg.go): two on messages (INSERT, UPDATE) and two
// on message_bodies (INSERT, UPDATE), all of which reference content_changed_at
// and must be dropped before SQLite will allow the column itself to be dropped.
// Only three of them exist on a SQLite database created from schema.sql, whose
// column DEFAULT stamps inserts and where EnsureTriggers therefore skips
// trg_messages_content_changed_ins; the drops below are IF EXISTS for that
// reason.
var contentChangedAtTriggerNames = []struct {
	name  string
	table string
}{
	{"trg_messages_content_changed_ins", "messages"},
	{"trg_messages_content_changed_at", "messages"},
	{"trg_message_bodies_content_changed_ins", "message_bodies"},
	{"trg_message_bodies_content_changed_upd", "message_bodies"},
}

// dropContentChangedAtColumn tears content_changed_at back out of a store
// built by testutil.NewTestStore, reproducing the shape of an archive that
// predates the column, on both backends. SQLite refuses ALTER TABLE ... DROP
// COLUMN while any trigger or index still references the column, so the
// removal must happen in dependency order: triggers first, then the index,
// then the column. DROP TRIGGER syntax differs by backend -- PostgreSQL
// triggers are namespaced per-table and require "ON <table>"; SQLite triggers
// are named at the schema level and reject a table clause.
func dropContentChangedAtColumn(t *testing.T, st *store.Store) {
	t.Helper()
	for _, trg := range contentChangedAtTriggerNames {
		stmt := `DROP TRIGGER IF EXISTS ` + trg.name
		if st.IsPostgreSQL() {
			stmt += ` ON ` + trg.table
		}
		_, err := st.DB().Exec(stmt)
		require.NoErrorf(t, err, "drop trigger %s", trg.name)
	}
	_, err := st.DB().Exec(`DROP INDEX IF EXISTS idx_messages_content_changed_at`)
	require.NoError(t, err, "drop idx_messages_content_changed_at")
	_, err = st.DB().Exec(`ALTER TABLE messages DROP COLUMN content_changed_at`)
	require.NoError(t, err, "drop content_changed_at column")
}

// contentChangedBackfillMigration is the ledger name InitSchema records once the
// content_changed_at backfill has run.
const contentChangedBackfillMigration = "messages_content_changed_at_backfill"

// clearContentChangedBackfillLedger deletes that row from applied_migrations so
// InitSchema treats the migration as never having run -- the ledger state of
// an archive from before the migration shipped.
func clearContentChangedBackfillLedger(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), contentChangedBackfillMigration)
	require.NoErrorf(t, err, "clear migration ledger entry %s", contentChangedBackfillMigration)
}

// TestContentChangedAt_UpgradeFromDatabaseWithoutColumn proves the migration an
// existing archive actually performs: a database whose messages table predates
// content_changed_at gains the column, has every existing row seeded from
// last_modified, has working triggers afterwards, and -- the part a
// fresh-schema test cannot cover -- stamps rows INSERTED AFTER the upgrade.
// Without an INSERT trigger those rows would be NULL forever, because neither
// ADD COLUMN carries a default, and they would never appear in the feed.
//
// A store from testutil.NewTestStore already has the column, its index, the
// triggers this backend creates for it, and the backfill's ledger row, so the
// naive version of this test would find nothing to upgrade: InitSchema would
// skip the backfill (ledger already marked applied) and the ADD COLUMN
// migration would be a silent no-op (IsDuplicateColumnError).
// dropContentChangedAtColumn and clearContentChangedBackfillLedger reconstruct
// the pre-upgrade shape first.
func TestContentChangedAt_UpgradeFromDatabaseWithoutColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)
	stampLastModified(t, st, id, "2020-01-02 03:04:05")

	// Tear the column back out, in dependency order.
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)

	require.NoError(st.InitSchema())

	// (a) existing rows seeded from last_modified, not from "now"
	assert.Contains(readContentChangedAt(t, st, id), "2020-01-02",
		"backfill must seed from last_modified")

	// (b) rows inserted after the upgrade are stamped
	fresh := seedMessage(t, st, 2)
	assert.NotEmpty(readContentChangedAt(t, st, fresh),
		"the INSERT trigger must stamp rows created after an upgrade")

	// (c) triggers actually work after the upgrade
	base := stampContentChangedAt(t, st, id)
	require.NoError(updateMessageColumn(t, st, id, "subject"))
	assert.NotEqual(base, readContentChangedAt(t, st, id),
		"the UPDATE trigger must work after an upgrade")

	// (d) the backfill records itself so reopens do not rescan the table
	applied, err := st.IsMigrationApplied(contentChangedBackfillMigration)
	require.NoError(err)
	assert.True(applied)
}

// TestContentChangedAt_BackfillNeverMintsANullWatermark covers the values
// SQLite's strftime refuses.
//
// The backfill seeds content_changed_at from last_modified. strftime returns
// NULL rather than an error for any input its parser rejects — a unix integer,
// an empty string, anything malformed — and last_modified is a DATETIME text
// column SQLite does not type-check. A NULL watermark is terminal: the feed's
// range predicate excludes NULL, and the migration ledger guarantees the
// `WHERE content_changed_at IS NULL` scan never runs again, so the row is
// invisible to the feed forever.
//
// PostgreSQL cannot fail this way — last_modified is a real TIMESTAMPTZ there
// and the backfill copies it without conversion — so this is SQLite-only.
func TestContentChangedAt_BackfillNeverMintsANullWatermark(t *testing.T) {
	testutil.SkipIfPostgres(t, "only SQLite's untyped DATETIME text can hold a value strftime rejects")
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	// One row per value strftime cannot parse, plus a parseable control so the
	// test still proves the backfill seeds from last_modified when it can.
	unparseable := map[string]int64{
		"1700000000":          seedMessage(t, st, 1), // a unix epoch integer
		"":                    seedMessage(t, st, 2), // an empty string
		"not a time":          seedMessage(t, st, 3), // free text
		"2020-13-45":          seedMessage(t, st, 4), // structurally plausible, out of range
		"2020-01-02 03:04:05": seedMessage(t, st, 5), // the control: parseable
	}
	for value, id := range unparseable {
		stampLastModified(t, st, id, value)
	}

	// Reconstruct an archive that predates the column so InitSchema really runs
	// the backfill over these rows.
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	require.NoError(st.InitSchema())

	for value, id := range unparseable {
		assert.Truef(readRawContentChangedAt(t, st, id).Valid,
			"message %d has last_modified %q, which strftime maps to NULL: the backfill has "+
				"already marked itself applied, so nothing will ever stamp this row and it can "+
				"never appear in the change feed", id, value)
	}
	assert.Contains(readContentChangedAt(t, st, unparseable["2020-01-02 03:04:05"]), "2020-01-02",
		"a parseable last_modified must still seed the watermark")

	// The feed itself is the point: every seeded row has to be reachable from a
	// zero cursor. Settle the clock first — the fallback stamps "now", and the
	// feed deliberately withholds the instant it is reading in.
	settleFeedClock(t, st)
	page, err := st.ListChangedMessages(context.Background(), time.Time{}, 0, 100)
	require.NoError(err)
	seen := map[int64]bool{}
	for _, m := range page.Messages {
		seen[m.ID] = true
	}
	for value, id := range unparseable {
		assert.Truef(seen[id], "message %d (last_modified %q) is missing from the change feed", id, value)
	}
}

// TestContentChangedAt_MessageInsertedDuringUpgradeIsStamped closes the window
// between the backfill and the triggers. The backfill records itself in the
// migration ledger the moment it finishes and never looks for NULL watermarks
// again, while InitSchema still has whole-table index builds ahead of it — on a
// large archive, minutes of them. A message written by another connection in
// that window used to land with content_changed_at NULL, which is terminal: the
// feed's range predicate excludes NULL and the ledger gate means nothing will
// ever fix it. Creating the triggers before the backfill makes the INSERT
// trigger the writer for that row instead.
//
// The insert is performed by a test-only hook placed at exactly that point in
// InitSchema, so the race is reproduced rather than raced.
func TestContentChangedAt_MessageInsertedDuringUpgradeIsStamped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	existing := seedMessage(t, st, 1)
	stampLastModified(t, st, existing, "2020-01-02 03:04:05")

	// Reconstruct an archive that predates the column, so InitSchema really
	// runs the migration, the backfill, and the index builds.
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)

	var concurrent int64
	restore := store.SetInitSchemaWindowHookForTest(func() {
		concurrent = seedMessage(t, st, 2)
	})
	defer restore()
	require.NoError(st.InitSchema())
	restore()

	require.NotZero(concurrent,
		"the hook never fired, so this test proves nothing about the window")
	assert.Truef(readRawContentChangedAt(t, st, concurrent).Valid,
		"message %d was inserted while InitSchema was still building indexes and "+
			"has a NULL content_changed_at: the backfill has already marked itself "+
			"applied, so nothing will ever stamp it and the row can never appear in "+
			"the change feed", concurrent)
}

// TestContentChangedAt_ColumnOrderMatchesAfterUpgrade guards against a fresh
// database and an upgraded one declaring messages' columns in different orders.
// ALTER TABLE always appends, so a column placed mid-table in schema.sql but
// appended by the migration diverges, and any read that goes by position writes
// or interprets values in the wrong columns. This asserts content_changed_at
// lands in the same position either way.
//
// subset.go's messages copy used to be exactly such a reader
// ("INSERT INTO messages SELECT * FROM src.messages"); it now names the columns
// the source and destination share, so it no longer relies on this. The two
// layouts still meet — a subset's source and destination are routinely one
// fresh and one upgraded — so the invariant is worth pinning on its own.
//
// Scoped to content_changed_at on purpose: last_modified and embed_gen already
// disagree at this commit (a pre-existing upstream defect out of scope for this
// change), so asserting the whole column order would fail for reasons this
// work did not cause.
func TestContentChangedAt_ColumnOrderMatchesAfterUpgrade(t *testing.T) {
	require := require.New(t)

	fresh := testutil.NewTestStore(t)
	freshCols, err := store.MessagesTableColumns(fresh)
	require.NoError(err)

	upgraded := testutil.NewTestStore(t)
	dropContentChangedAtColumn(t, upgraded)
	clearContentChangedBackfillLedger(t, upgraded)
	require.NoError(upgraded.InitSchema())
	upgradedCols, err := store.MessagesTableColumns(upgraded)
	require.NoError(err)

	// slices.Index returns -1 for "not found", and -1 == -1 would make the
	// position assertion below pass vacuously if the column were missing from
	// both schemas instead of proving it lands in the same real position.
	freshIdx := slices.Index(freshCols, "content_changed_at")
	upgradedIdx := slices.Index(upgradedCols, "content_changed_at")
	require.GreaterOrEqual(freshIdx, 0, "content_changed_at must exist in the fresh schema")
	require.GreaterOrEqual(upgradedIdx, 0, "content_changed_at must exist in the upgraded schema")

	assert.Equal(t, freshIdx, upgradedIdx,
		"content_changed_at must occupy the same column position on fresh and upgraded databases; "+
			"a divergence silently corrupts any read of a message row that goes by position")
}

// contentChangedStampShape is the fixed-width text layout SQLite watermarks
// must have. The feed's cursor comparison is lexical on SQLite, so a stamp of a
// different width sorts into the wrong place and is skipped or repeated
// forever.
var contentChangedStampShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}$`)

// TestContentChangedAt_FreshAndUpgradedStampsShareOneFormat pins the two
// writers of a SQLite watermark against each other. A fresh database stamps new
// rows from the column DEFAULT in schema.sql; a database upgraded by ALTER
// TABLE cannot carry that DEFAULT (SQLite rejects a non-constant one there) and
// stamps from the INSERT trigger instead. Both must produce the identical
// layout, because the two databases can meet — subset.go copies messages
// between them and the cursor comparison is lexical.
func TestContentChangedAt_FreshAndUpgradedStampsShareOneFormat(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL compares TIMESTAMPTZ natively and has one stamping writer")
	require := require.New(t)
	assert := assert.New(t)

	fresh := testutil.NewTestStore(t)
	freshStamp := readContentChangedAt(t, fresh, seedMessage(t, fresh, 1))

	upgraded := testutil.NewTestStore(t)
	dropContentChangedAtColumn(t, upgraded)
	clearContentChangedBackfillLedger(t, upgraded)
	require.NoError(upgraded.InitSchema())
	upgradedStamp := readContentChangedAt(t, upgraded, seedMessage(t, upgraded, 1))

	assert.Regexp(contentChangedStampShape, freshStamp,
		"a fresh database's watermark must carry the millisecond layout the cursor sorts on")
	assert.Regexp(contentChangedStampShape, upgradedStamp,
		"an upgraded database's watermark must carry the same layout as a fresh one")
	assert.Len(upgradedStamp, len(freshStamp),
		"fresh and upgraded stamps must be the same width or lexical cursor comparison breaks")
}

// insertMessagesTriggerPrograms counts the trigger subprograms SQLite compiles
// into an INSERT on messages. The `Program` opcode is how the bytecode invokes a
// row trigger, and its presence — not the trigger body actually running — is
// what forces SQLite to open a statement journal per INSERT.
func insertMessagesTriggerPrograms(t *testing.T, st *store.Store, insert string, args ...any) int {
	t.Helper()
	rows, err := st.DB().Query("EXPLAIN "+insert, args...)
	require.NoError(t, err, "EXPLAIN insert")
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	opcodeCol := slices.Index(cols, "opcode")
	require.GreaterOrEqual(t, opcodeCol, 0, "EXPLAIN must report an opcode column")

	cells := make([]sql.NullString, len(cols))
	targets := make([]any, len(cols))
	for i := range cells {
		targets[i] = &cells[i]
	}
	programs := 0
	for rows.Next() {
		require.NoError(t, rows.Scan(targets...))
		if cells[opcodeCol].String == "Program" {
			programs++
		}
	}
	require.NoError(t, rows.Err())
	return programs
}

// TestContentChangedAt_InsertRunsNoTriggerOnAFreshDatabase keeps message ingest
// at the cost it had before the watermark existed.
//
// SQLite triggers cannot assign to NEW, so an AFTER INSERT trigger that stamps
// the watermark has to re-UPDATE the row that was just inserted. Worse, merely
// HAVING a row trigger on messages makes SQLite compile a trigger subprogram
// into every INSERT and open a statement journal for it — measured at 6.4s
// against 1.1s for a 100k-row bulk insert even with the trigger's WHEN guard
// never once satisfied. Every importer and internal/fakevault runs through this
// path. The column DEFAULT stamps fresh databases instead, and EnsureTriggers
// omits the INSERT trigger entirely when that DEFAULT is present.
//
// total_changes() is SQLite's per-connection count of rows written, and unlike
// changes() it does include rows written by trigger programs — which is exactly
// what has to be counted.
func TestContentChangedAt_InsertRunsNoTriggerOnAFreshDatabase(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL stamps in a BEFORE trigger, which needs no second write")
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "insert-cost@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "insert-cost", "email_thread", "Insert cost")
	require.NoError(err)

	const insert = `INSERT INTO messages (source_id, source_message_id, conversation_id, message_type, subject)
		 VALUES (?,?,?,?,?)`

	assert.Zero(insertMessagesTriggerPrograms(t, st, insert, src.ID, "explain-only", conv, "email", "x"),
		"an INSERT into messages must compile no trigger subprogram on a fresh database: "+
			"SQLite opens a statement journal for every INSERT that has one, whether or not the "+
			"trigger body runs")

	// One pinned connection: total_changes() is per-connection and the pool
	// would otherwise hand the two readings out of different sessions.
	ctx := context.Background()
	conn, err := st.DB().Conn(ctx)
	require.NoError(err)
	defer func() { _ = conn.Close() }()

	totalChanges := func() int64 {
		var n int64
		require.NoError(conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&n))
		return n
	}

	before := totalChanges()
	_, err = conn.ExecContext(ctx, insert, src.ID, "insert-cost-1", conv, "email", "insert cost")
	require.NoError(err)
	written := totalChanges() - before

	var stamp sql.NullString
	require.NoError(conn.QueryRowContext(ctx,
		`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE source_message_id = 'insert-cost-1'`).
		Scan(&stamp))
	require.True(stamp.Valid, "the inserted row must still get a watermark")
	assert.Regexp(contentChangedStampShape, stamp.String)

	assert.EqualValues(1, written,
		"inserting one message must write one row; the watermark has to come from the column "+
			"DEFAULT rather than from an AFTER INSERT trigger that re-UPDATEs the row")
}

// TestContentChangedAt_UpgradedDatabaseKeepsTheInsertTrigger is the other half
// of the DEFAULT optimisation. A database upgraded by ALTER TABLE ADD COLUMN
// cannot carry a non-constant DEFAULT, so dropping the INSERT trigger there
// would leave every row inserted after the upgrade with a NULL watermark —
// permanently invisible to the feed, since the backfill has already marked
// itself applied.
func TestContentChangedAt_UpgradedDatabaseKeepsTheInsertTrigger(t *testing.T) {
	testutil.SkipIfPostgres(t, "the DEFAULT/trigger split is a SQLite ALTER TABLE limitation")
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	require.NoError(st.InitSchema())

	src, err := st.GetOrCreateSource("gmail", "upgraded@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "upgraded", "email_thread", "Upgraded")
	require.NoError(err)
	_, err = st.DB().Exec(
		`INSERT INTO messages (source_id, source_message_id, conversation_id, message_type)
		 VALUES (?,?,?,?)`, src.ID, "upgraded-1", conv, "email")
	require.NoError(err)

	var stamp sql.NullString
	require.NoError(st.DB().QueryRow(
		`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE source_message_id = 'upgraded-1'`).
		Scan(&stamp))
	require.True(stamp.Valid,
		"a row inserted after an ALTER TABLE upgrade must still be stamped: the column has no "+
			"DEFAULT there, so the INSERT trigger is the only writer")
	assert.Regexp(contentChangedStampShape, stamp.String)
}

// TestContentChangedAt_NullWatermarkIsStamped pins the null-safe comparison in
// the UPDATE trigger's yield guard. With `=` (SQLite) or `=`/`<>` (PostgreSQL)
// instead of IS / IS NOT DISTINCT FROM, `OLD.content_changed_at = NEW.content_changed_at`
// evaluates to NULL for a row whose watermark is NULL, the WHEN clause is never
// satisfied, and that row is stranded outside the feed forever. Rows can carry a
// NULL watermark on an archive copied or restored from a pre-backfill database.
func TestContentChangedAt_NullWatermarkIsStamped(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)

	// Naming only content_changed_at keeps the UPDATE OF list from matching, so
	// this write establishes the NULL precondition instead of being re-stamped.
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET content_changed_at = NULL WHERE id = ?`), id)
	require.NoError(t, err, "clear content_changed_at")

	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "changed subject", id)
	require.NoError(t, err, "update subject")

	// readContentChangedAt fails on NULL, which is the assertion.
	assert.NotEmpty(t, readContentChangedAt(t, st, id),
		"a content update must stamp a row whose watermark is NULL: the guard has to be "+
			"null-safe or the row never rejoins the feed")
}
