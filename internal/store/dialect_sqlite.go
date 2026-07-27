package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// SQLiteTimestampLayout is the Go layout matching strftime('%Y-%m-%d %H:%M:%f').
const SQLiteTimestampLayout = "2006-01-02 15:04:05.000"

// SQLiteDialect implements Dialect for SQLite (the default backend).
//
// The zero value is ready to use, and every method except ReadWatermarkBounds
// is stateless — callers outside Store that only want Rebind or BuildFTSArg can
// go on constructing one per call. ReadWatermarkBounds remembers the newest
// instant it has proved the database had no write in flight, which the Store's
// single long-lived instance accumulates across pages.
type SQLiteDialect struct {
	quiescentMu sync.Mutex
	quiescentAt time.Time
}

func (d *SQLiteDialect) DriverName() string { return sqliteutil.DriverName() }

// Rebind is a no-op for SQLite — it uses ? placeholders natively.
func (d *SQLiteDialect) Rebind(query string) string { return query }

// Now returns the SQLite expression for the current UTC timestamp.
func (d *SQLiteDialect) Now() string { return "datetime('now')" }

// ContentChangedNow returns the SQLite expression that stamps
// content_changed_at at millisecond resolution. strftime's %f gives
// milliseconds as a floor on collision spacing, not a guarantee of
// distinctness, but it is the finest resolution SQLite's DATETIME text
// format supports, and the trigger's WHEN guard plus the (content_changed_at,
// id) cursor tolerate ties.
func (d *SQLiteDialect) ContentChangedNow() string {
	return `strftime('%Y-%m-%d %H:%M:%f','now')`
}

// TimestampParam formats t to match ContentChangedNow's textual format.
// SQLite's driver otherwise serialises time.Time with a "+00:00" suffix,
// which sorts BELOW an equal stored value under lexical comparison and
// would silently drop every row sharing the cursor's instant.
func (d *SQLiteDialect) TimestampParam(t time.Time) any {
	if t.IsZero() {
		return "" // sorts below every stored timestamp: "from the beginning"
	}
	return t.UTC().Format(SQLiteTimestampLayout)
}

// sqliteQuiescentProbeTimeout is how long ReadWatermarkBounds waits for the
// SQLite write lock before giving up on advancing the bound for this page. It
// is deliberately short: a page that waits is a consumer that waits, and the
// fallback (the newest instant the database was already proved quiescent at)
// costs only freshness, never correctness. SQLite write transactions are
// normally sub-millisecond, so 250ms times out only against a writer that is
// genuinely holding the lock — which is exactly the case where waiting longer
// would not have helped either.
//
// The probe is a WRITE-lock acquisition, so polling the feed costs the database
// writer throughput in a way an ordinary read does not: measured on one machine
// against three concurrent writers, eight clients paging the feed in a tight
// loop cut writes to 15% of the unloaded rate, where eight clients running an
// equivalent plain SELECT left 49%. One consumer polling once a second is free;
// a consumer that polls as fast as it can is competing with the importer for
// the write lock. Poll on an interval, and use has_more (not a tighter poll) to
// drain a backlog.
const sqliteQuiescentProbeTimeout = 250 * time.Millisecond

// ReadWatermarkBounds implements Dialect.
//
// SQLite has no pg_stat_activity: nothing exposes when another connection's
// write transaction began, or whether one is open at all. What it has instead
// is a single writer. Acquiring the write lock is therefore a proof rather than
// an observation — while this probe holds it, no other write transaction
// exists, so every content_changed_at stamp in the database has committed. The
// clock read inside that lock is a valid commit bound:
//
//   - A write that committed before the lock was acquired stamped itself
//     earlier still, so it is strictly below the reading (or equal to it, which
//     the page's strict `<` also excludes — a delay, not a loss).
//   - A write that starts after the probe releases the lock cannot be stamped
//     before it acquires the lock, which is after the reading.
//
// When the lock cannot be taken within sqliteQuiescentProbeTimeout, a writer is
// in flight and its start time is unknowable, so the bound falls back to the
// newest instant this dialect has already proved quiescent — the last probe
// that succeeded. That is always safe (any write in flight now began after it)
// and it is why the instant is remembered rather than recomputed: the fallback
// is the whole liveness story on SQLite. The feed then stops advancing until
// the writer finishes, and says so through the lag between CommitBound and Now.
//
// A fresh dialect that has never completed a probe reports the zero time, so
// the feed publishes nothing until it first sees the database idle. That is the
// honest answer — it has no evidence any stamp has committed — and it resolves
// on the first quiet moment.
//
// The probe holds the write lock for one clock read, and commits nothing, so it
// writes no WAL frames.
func (d *SQLiteDialect) ReadWatermarkBounds(
	ctx context.Context, db *sql.DB,
) (WatermarkBounds, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return WatermarkBounds{}, fmt.Errorf("read change-feed watermark bounds: %w", err)
	}
	defer func() { _ = conn.Close() }()

	quiescent, proved, err := d.probeQuiescentInstant(ctx, conn)
	if err != nil {
		return WatermarkBounds{}, err
	}
	// A successful probe already read the clock, under the write lock; that
	// reading is this call's server_time as much as a bare SELECT would be, and
	// reusing it keeps Now and CommitBound from disagreeing by a millisecond
	// for no reason. Only a probe that timed out needs the clock separately.
	now := quiescent
	if !proved {
		if now, err = d.readClock(ctx, conn); err != nil {
			return WatermarkBounds{}, err
		}
	}

	d.quiescentMu.Lock()
	defer d.quiescentMu.Unlock()
	if proved {
		// The MOST RECENT proof, not the greatest one. They differ only if the
		// database clock steps backwards, and there the greatest is the wrong
		// answer: it would stand above stamps taken after the step, which may
		// still be in flight.
		d.quiescentAt = quiescent
	}
	bound := d.quiescentAt
	if bound.After(now) {
		// Only reachable if the database clock stepped backwards between two
		// probes, which breaks the watermark itself and is outside what this
		// bound can repair (docs/api-server.md says so). Hold the published
		// invariant — CommitBound is never after Now — rather than emit a pair
		// that contradicts the contract on top of it.
		bound = now
	}
	return WatermarkBounds{Now: now, CommitBound: bound}, nil
}

// probeQuiescentInstant takes the SQLite write lock, reads the clock under it,
// and releases it. The second return is false when a writer held the lock for
// longer than the probe was willing to wait — not an error, just no new
// evidence.
func (d *SQLiteDialect) probeQuiescentInstant(
	ctx context.Context, conn *sql.Conn,
) (time.Time, bool, error) {
	restore, err := d.useProbeBusyTimeout(ctx, conn)
	if err != nil {
		return time.Time{}, false, err
	}
	defer restore()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if d.IsBusyError(err) {
			return time.Time{}, false, nil
		}
		if isSQLiteError(err, "readonly") {
			// A read-only handle can never take the write lock, so it can never
			// establish the bound — and it cannot assume there is no writer
			// either, because another process may hold the same file open for
			// writing. Say so instead of serving a feed that silently returns
			// nothing.
			return time.Time{}, false, fmt.Errorf(
				"read change-feed watermark bounds: the content-change feed needs a "+
					"writable database handle to establish how far writes have "+
					"committed: %w", err)
		}
		return time.Time{}, false, fmt.Errorf("read change-feed watermark bounds: %w", err)
	}

	stamp, err := d.readClock(ctx, conn)
	if err != nil {
		d.rollback(ctx, conn)
		return time.Time{}, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		d.rollback(ctx, conn)
		return time.Time{}, false, fmt.Errorf("release change-feed watermark probe: %w", err)
	}
	return stamp, true, nil
}

// useProbeBusyTimeout narrows this connection's busy timeout to the probe's,
// returning a function that puts the connection's own value back. The
// connection returns to the pool afterwards, so leaving the probe's timeout on
// it would silently shorten every unrelated statement that later borrows it.
func (d *SQLiteDialect) useProbeBusyTimeout(ctx context.Context, conn *sql.Conn) (func(), error) {
	var configured int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&configured); err != nil {
		return nil, fmt.Errorf("read busy timeout for change-feed watermark probe: %w", err)
	}
	set := func(c context.Context, ms int64) error {
		_, err := conn.ExecContext(c, fmt.Sprintf("PRAGMA busy_timeout = %d", ms))
		return err
	}
	// Narrow, never widen: a store configured to give up on a busy database
	// sooner than this means it, and the probe has a safe fallback either way.
	if err := set(ctx, min(configured, sqliteQuiescentProbeTimeout.Milliseconds())); err != nil {
		return nil, fmt.Errorf("set busy timeout for change-feed watermark probe: %w", err)
	}
	return func() {
		// WithoutCancel: the connection must be handed back with its own
		// timeout even when the caller's context has already expired.
		_ = set(context.WithoutCancel(ctx), configured)
	}, nil
}

// rollback releases a probe transaction that could not be committed. It runs on
// an uncancellable context so a cancelled request cannot return a connection to
// the pool with the write lock still held.
func (d *SQLiteDialect) rollback(ctx context.Context, conn *sql.Conn) {
	_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
}

// readClock reads the database clock in exactly the format the triggers stamp,
// so the reading and the watermarks it bounds are comparable.
func (d *SQLiteDialect) readClock(ctx context.Context, conn *sql.Conn) (time.Time, error) {
	var stamp nullableTimestamp
	if err := conn.QueryRowContext(ctx, "SELECT "+d.ContentChangedNow()).Scan(&stamp); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	if !stamp.Valid {
		return time.Time{}, errors.New("read database clock: no value returned")
	}
	return stamp.Time.UTC(), nil
}

// InsertOrIgnore is a no-op for SQLite — the syntax is native.
func (d *SQLiteDialect) InsertOrIgnore(sql string) string { return sql }

// BoolTrueExpr returns "col = 1" — SQLite stores booleans as 0/1 INTEGER.
func (d *SQLiteDialect) BoolTrueExpr(col string) string { return col + " = 1" }

// JSONBindExpr is "?" on SQLite — JSON columns are plain TEXT.
func (d *SQLiteDialect) JSONBindExpr() string { return "?" }

// BuildFTSArg formats search terms as an FTS5 MATCH argument: each
// term double-quote-escaped, suffixed with "*" for prefix match, and
// space-joined (FTS5 treats space as implicit AND). Embedded "*" is
// stripped first so user input cannot break the trailing prefix
// operator. Matches the shape produced by the query package's
// SQLiteQueryDialect.BuildFTSTerm so the API search path and the
// engine deep-search path return the same hits for the same input —
// searching "invo" must match "invoice" in both paths.
//
// Terms that would tokenize to nothing under the default FTS5
// tokenizer (no Unicode letter or digit — e.g. "!!!", "---", "") are
// dropped. If all terms drop, returns "" so the caller can
// short-circuit instead of dispatching a malformed FTS5 MATCH that
// errors at the driver. Mirrors the empty-fallback shape in
// PostgreSQLDialect.BuildFTSArg.
func (d *SQLiteDialect) BuildFTSArg(terms []string) string {
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		if !hasFTSToken(t) {
			continue
		}
		t = strings.ReplaceAll(t, `"`, `""`)
		t = strings.ReplaceAll(t, "*", "")
		quoted = append(quoted, `"`+t+`"*`)
	}
	return strings.Join(quoted, " ")
}

// hasFTSToken reports whether s contains at least one rune that the
// default FTS5 tokenizer (unicode61) would emit as part of a token —
// i.e., a Unicode letter or digit. Punctuation-only strings tokenize
// to nothing, so a MATCH built from them is a syntax error.
func hasFTSToken(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// InsertOrIgnorePrefix is a no-op for SQLite — OR IGNORE stays in the prefix.
func (d *SQLiteDialect) InsertOrIgnorePrefix(sql string) string { return sql }

// InsertOrIgnoreSuffix returns "" for SQLite — OR IGNORE is in the statement prefix.
func (d *SQLiteDialect) InsertOrIgnoreSuffix() string { return "" }

// FTSUpsert inserts or replaces an FTS5 row. FTS5 requires rowid to be
// specified explicitly so the virtual table's rowid matches messages.id;
// the dialect owns this detail so callers don't pass messageID twice.
func (d *SQLiteDialect) FTSUpsert(q querier, doc FTSDoc) error {
	_, err := q.Exec(
		`INSERT OR REPLACE INTO messages_fts(rowid, message_id, subject, body, from_addr, to_addr, cc_addr)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		doc.MessageID, doc.MessageID, doc.Subject, doc.Body,
		doc.FromAddr, doc.ToAddrs, doc.CcAddrs,
	)
	return err
}

// FTSSearchClause returns SQL fragments for FTS5 full-text search.
//
// The bm25 weights approximate PostgreSQL's setweight field-priority
// preferences (subject heaviest, then sender, then body / other
// recipients) for typical email shapes. PostgreSQL assigns recipients
// weight C and body weight D so body-only search can distinguish them,
// then supplies explicit rank weights that keep C and D equivalent. This is a
// best-effort SQLite tuning, NOT a strict cross-backend parity guarantee.
//
// Weights are positional over every column declared in messages_fts —
// UNINDEXED columns count too even though they cannot match — so the
// leading 1.0 is the placeholder for `message_id UNINDEXED`. The
// remaining slots map to (subject, body, from_addr, to_addr, cc_addr).
// PostgreSQL applies setweight 'A'=1.0 to subject and 'B'=0.4 to sender,
// with explicit C/D rank weights of 0.1 for recipients/body — a 10:4:1 ratio,
// which bm25 reproduces as 10/1/4/1/1 across (subject, body, from, to,
// cc). bm25 returns lower (more negative) scores for more relevant rows,
// so callers ORDER BY this expression ascending (the default).
//
// Known divergence: SQLite's bm25() applies Okapi BM25 document-length
// normalization while PostgreSQL's default ts_rank() does not, so very
// long subject-hit documents can still rank below short body-hit
// documents on SQLite while PG ranks them subject-first. See the
// docs-site search ranking page ("Where Ordering Can Diverge") and
// TestFTSRank_KnownDivergence for the expected-behavior pin and
// rationale.
func (d *SQLiteDialect) FTSSearchClause() (join, where, orderBy string, orderArgCount int) {
	return "JOIN messages_fts ON messages_fts.rowid = m.id",
		"messages_fts MATCH ?",
		"bm25(messages_fts, 1.0, 10.0, 1.0, 4.0, 1.0, 1.0)",
		0
}

// FTSDeleteSQL returns the SQL to delete a message's FTS5 entry.
func (d *SQLiteDialect) FTSDeleteSQL() string {
	return `DELETE FROM messages_fts WHERE message_id IN (
		SELECT id FROM messages WHERE source_id = ?
	)`
}

func (d *SQLiteDialect) InvalidateFTSForMessage(q querier, messageID int64) error {
	_, err := q.Exec("DELETE FROM messages_fts WHERE rowid = ?", messageID)
	if d.IsNoSuchTableError(err) {
		// A missing FTS table cannot contain a stale searchable row. Preserve
		// the existing best-effort indexing contract so canonical message
		// persistence can continue and a later rebuild can recreate the index.
		return nil
	}
	return err
}

// FTSBackfillBatchSQL returns the SQL to backfill FTS5 for a range of message IDs.
// Parameters: fromID(?), toID(?)
func (d *SQLiteDialect) FTSBackfillBatchSQL() string {
	return `INSERT OR REPLACE INTO messages_fts (rowid, message_id, subject, body, from_addr, to_addr, cc_addr)
		SELECT m.id, m.id, COALESCE(m.subject, ''), COALESCE(mb.body_text, ''),
			COALESCE(
				CASE WHEN m.message_type != 'email' AND m.message_type IS NOT NULL AND m.message_type != ''
				     THEN (SELECT COALESCE(p.phone_number, p.email_address) FROM participants p WHERE p.id = m.sender_id)
				END,
				(SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'from'),
				''
			),
			COALESCE((SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'to'), ''),
			COALESCE((SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'cc'), '')
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.id >= ? AND m.id < ?`
}

// FTSAvailable probes for FTS5 by querying the virtual table.
// Checking sqlite_master alone is insufficient: a binary built without FTS5
// support will fail with "no such module: fts5" even if the table exists.
func (d *SQLiteDialect) FTSAvailable(db *sql.DB) bool {
	var probe int
	err := db.QueryRowContext(context.Background(), "SELECT 1 FROM messages_fts LIMIT 1").Scan(&probe)
	return err == nil || errors.Is(err, sql.ErrNoRows)
}

// FTSNeedsBackfill reports whether the FTS5 table needs population.
// Probes for the existence of ANY message lacking an FTS entry, matching the
// PostgreSQL EXISTS(search_fts IS NULL) semantics. The previous MAX(rowid)
// vs MAX(id) heuristic missed a hole left at a LOW id while later ids were
// indexed — reachable because UpsertFTS failures during sync are
// warn-and-continue (sync.go) while the message row still commits, so id N can
// be unindexed while N+1.. are indexed. messages_fts.rowid == messages.id and
// there are no triggers, so the NOT EXISTS join is rowid-served and cheap on
// FTS5 (no full body scan).
func (d *SQLiteDialect) FTSNeedsBackfill(db *sql.DB) bool {
	var exists bool
	if err := db.QueryRowContext(context.Background(),
		`SELECT EXISTS (
			SELECT 1 FROM messages m
			 WHERE NOT EXISTS (
			     SELECT 1 FROM messages_fts f WHERE f.rowid = m.id
			 )
		)`,
	).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// FTSNeedsBackfillQuick compares MAX(id) against MAX(rowid) — two B-tree
// lookups, instant at any archive size. It catches the dominant staleness
// (tail of the messages table not yet indexed: fresh import, interrupted
// backfill) but misses interior holes; FTSNeedsBackfill stays authoritative.
func (d *SQLiteDialect) FTSNeedsBackfillQuick(db *sql.DB) bool {
	var msgMax int64
	if err := db.QueryRowContext(context.Background(),
		"SELECT COALESCE(MAX(id), 0) FROM messages",
	).Scan(&msgMax); err != nil || msgMax == 0 {
		return false
	}
	var ftsMax int64
	if err := db.QueryRowContext(context.Background(),
		"SELECT COALESCE(MAX(rowid), 0) FROM messages_fts",
	).Scan(&ftsMax); err != nil {
		return false
	}
	return ftsMax < msgMax
}

// FTSClearSQL returns the SQL to clear all FTS5 data.
func (d *SQLiteDialect) FTSClearSQL() string {
	return "DELETE FROM messages_fts"
}

// SchemaFTS returns the embedded filename containing FTS5 virtual table DDL.
func (d *SQLiteDialect) SchemaFTS() string {
	return "schema_sqlite.sql"
}

// FTSRebuildSchema drops and recreates the messages_fts virtual table. The
// DROP pathway discards FTS5 shadow tables in their entirety, which is the
// only reliable fix when those shadow tables are malformed — the `rebuild`
// pragma reads from them and `delete-all` is rejected on contentful tables.
//
// Runs on the querier so RebuildFTS can route it through the maintenance
// transaction (finding S1). SQLite DDL is transactional, so DROP/CREATE of
// the virtual table run fine inside the tx runMaintenance opens; SQLite has
// no statement_timeout, so the hatch is a plain transaction here.
func (d *SQLiteDialect) FTSRebuildSchema(q querier) error {
	if _, err := q.Exec("DROP TABLE IF EXISTS messages_fts"); err != nil {
		return fmt.Errorf("drop messages_fts: %w", err)
	}
	schema, err := schemaFS.ReadFile("schema_sqlite.sql")
	if err != nil {
		return fmt.Errorf("read schema_sqlite.sql: %w", err)
	}
	if _, err := q.Exec(string(schema)); err != nil {
		if d.IsNoSuchModuleError(err) {
			return errors.New("cannot rebuild FTS: this msgvault binary was built without " +
				"FTS5 support (rebuild with `-tags fts5`)",
			)
		}
		return fmt.Errorf("create messages_fts: %w", err)
	}
	return nil
}

// EnsureFTSIndex is a no-op for SQLite: its messages_fts virtual table (and
// the index it implies) is created via the SchemaFTS file during InitSchema,
// not a post-migration step (cr2-10).
func (d *SQLiteDialect) EnsureFTSIndex(querier) error { return nil }

// EnsureTriggers creates the content_changed_at maintenance triggers.
//
// The last_modified triggers are NOT here: they are CREATE TRIGGER IF NOT
// EXISTS in schema.sql, which InitSchema re-execs on every open, and their
// definition is unchanged by this feature.
//
// content_changed_at's triggers are built here because their column list comes
// from MessagesContentColumns, shared with the PostgreSQL dialect so the two
// backends cannot drift, and because DROP + CREATE can replace a definition on
// an existing archive where CREATE TRIGGER IF NOT EXISTS silently would not.
func (d *SQLiteDialect) EnsureTriggers(q querier) error {
	cols := ContentChangedTriggerColumnList()
	guard := ContentChangedValueGuard("IS NOT")
	now := d.ContentChangedNow()
	insertStampedByDefault, err := d.contentChangedAtDefaultStamps(q)
	if err != nil {
		return err
	}
	stmts := []string{
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_ins`,
	}
	if !insertStampedByDefault {
		// Every new row gets a watermark. On a database upgraded by ALTER TABLE
		// ADD COLUMN this trigger is the only writer, because SQLite forbids a
		// non-constant DEFAULT there. A fresh database has the DEFAULT
		// (schema.sql) instead and this trigger is NOT created at all: SQLite
		// triggers cannot assign to NEW, so the stamp has to be a second
		// UPDATE of the row just inserted, and merely HAVING a row trigger on
		// messages forces SQLite to open a statement journal for every INSERT
		// -- measured at 6.4s versus 1.1s for a 100k-row bulk insert, with the
		// trigger body never once executing. The WHEN guard yields to an
		// explicit write in the INSERT rather than clobbering it.
		stmts = append(stmts, fmt.Sprintf(`CREATE TRIGGER trg_messages_content_changed_ins
		    AFTER INSERT ON messages FOR EACH ROW
		    WHEN NEW.content_changed_at IS NULL
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = NEW.id;
		    END`, now))
	}
	stmts = append(stmts,
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_at`,
		// UPDATE OF scopes to the columns the statement names; the value guard
		// then requires one of them to have actually changed. Recursion is
		// impossible: the trigger's own UPDATE touches only content_changed_at,
		// which is not in the column list. The IS guard is null-safe -- with
		// `=`, a NULL watermark is never stamped (measured).
		fmt.Sprintf(`CREATE TRIGGER trg_messages_content_changed_at
		    AFTER UPDATE OF %s ON messages FOR EACH ROW
		    WHEN OLD.content_changed_at IS NEW.content_changed_at AND %s
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = NEW.id;
		    END`, cols, guard, now),
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_ins`,
		fmt.Sprintf(`CREATE TRIGGER trg_message_bodies_content_changed_ins
		    AFTER INSERT ON message_bodies FOR EACH ROW
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = NEW.message_id;
		    END`, now),
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_upd`,
		// Value-guarded like the messages trigger: upsertMessageBody always
		// runs its ON CONFLICT DO UPDATE, even when messageBodyChanges reports
		// nothing changed, and PersistMessage calls it for every persisted
		// message. Unguarded, every resync would bump.
		fmt.Sprintf(`CREATE TRIGGER trg_message_bodies_content_changed_upd
		    AFTER UPDATE ON message_bodies FOR EACH ROW
		    WHEN OLD.body_text IS NOT NEW.body_text OR OLD.body_html IS NOT NEW.body_html
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = NEW.message_id;
		    END`, now),
	)
	for _, stmt := range stmts {
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("ensure content_changed_at triggers: %w", err)
		}
	}
	return nil
}

// contentChangedAtDefaultStamps reports whether messages.content_changed_at
// carries exactly the DEFAULT that ContentChangedNow writes, which is the case
// on a database created from schema.sql and impossible on one upgraded by
// ALTER TABLE ADD COLUMN.
//
// The comparison is exact rather than "has some default" on purpose: the INSERT
// trigger is only safe to omit when the DEFAULT produces the identical stamp
// format, since the change feed's cursor comparison is lexical. A default that
// has drifted from ContentChangedNow leaves the trigger in place, trading speed
// for a watermark the feed can still sort.
func (d *SQLiteDialect) contentChangedAtDefaultStamps(q querier) (bool, error) {
	var dflt sql.NullString
	err := q.QueryRow(
		`SELECT dflt_value FROM pragma_table_info('messages') WHERE name = 'content_changed_at'`,
	).Scan(&dflt)
	if errors.Is(err, sql.ErrNoRows) {
		// The column migration has not run yet on this handle; the caller's
		// trigger is then the only possible writer.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read messages.content_changed_at default: %w", err)
	}
	return dflt.Valid && dflt.String == d.ContentChangedNow(), nil
}

// LegacyColumnMigrations returns the ALTER TABLE ADD COLUMN statements that
// bring older SQLite databases up to the current schema. IsDuplicateColumnError
// silences these when the column already exists (idempotent migrations).
func (d *SQLiteDialect) LegacyColumnMigrations() []ColumnMigration {
	return []ColumnMigration{
		{`ALTER TABLE sources ADD COLUMN sync_config JSON`, "sync_config"},
		{`ALTER TABLE messages ADD COLUMN rfc822_message_id TEXT`, "rfc822_message_id"},
		{`ALTER TABLE sources ADD COLUMN oauth_app TEXT`, "oauth_app"},
		{`ALTER TABLE participants ADD COLUMN phone_number TEXT`, "phone_number"},
		{`ALTER TABLE participants ADD COLUMN canonical_id TEXT`, "canonical_id"},
		{`ALTER TABLE messages ADD COLUMN sender_id INTEGER REFERENCES participants(id)`, "sender_id"},
		{`ALTER TABLE messages ADD COLUMN message_type TEXT NOT NULL DEFAULT 'email'`, "message_type"},
		{`ALTER TABLE messages ADD COLUMN attachment_count INTEGER DEFAULT 0`, "attachment_count"},
		{`ALTER TABLE messages ADD COLUMN deleted_from_source_at DATETIME`, "deleted_from_source_at"},
		{`ALTER TABLE messages ADD COLUMN deleted_at DATETIME`, "deleted_at"},
		{`ALTER TABLE messages ADD COLUMN delete_batch_id TEXT`, "delete_batch_id"},
		{`ALTER TABLE conversations ADD COLUMN title TEXT`, "title"},
		{`ALTER TABLE conversations ADD COLUMN conversation_type TEXT NOT NULL DEFAULT 'email_thread'`, "conversation_type"},
		// embed_gen: per-message vector-embedding watermark. NULL default
		// means every legacy row reads as "needs embedding", which is
		// correct — the scan-and-fill worker (and backstop) will embed and
		// stamp them. No backfill.
		{`ALTER TABLE messages ADD COLUMN embed_gen INTEGER`, "embed_gen"},
		// last_modified: row-level last-modified watermark, the embed
		// worker's optimistic-CAS token. SQLite rejects a non-constant
		// DEFAULT in ADD COLUMN ("Cannot add a column with non-constant
		// default"), so the column is added with no default (existing rows
		// get NULL) and InitSchema's backfillLastModified follows up with a
		// one-shot `UPDATE ... SET last_modified = CURRENT_TIMESTAMP WHERE
		// last_modified IS NULL` so the CAS token is a comparable value
		// (NULL would never match `last_modified = ?`). Fresh DBs keep the
		// CREATE TABLE default in schema.sql, which IS allowed.
		{`ALTER TABLE messages ADD COLUMN last_modified DATETIME`, "last_modified"},
		// content_changed_at: content-scoped change watermark. No default here,
		// because SQLite rejects a non-constant DEFAULT in ADD COLUMN; fresh
		// databases DO carry one (schema.sql), and on this upgrade path the
		// INSERT trigger stamps new rows instead. InitSchema's backfill seeds
		// pre-existing rows from last_modified, a better starting point than
		// "now", which would make an existing archive look like every message
		// changed at upgrade time.
		{`ALTER TABLE messages ADD COLUMN content_changed_at DATETIME`, "content_changed_at"},
	}
}

// DatabaseSize returns the on-disk size of the SQLite database file.
// Returns (0, nil) for in-memory databases or when the file cannot be stat'd.
func (d *SQLiteDialect) DatabaseSize(_ *sql.DB, dbPath string) (int64, error) {
	if dbPath == "" || dbPath == ":memory:" || strings.Contains(dbPath, ":memory:") {
		return 0, nil
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return 0, nil //nolint:nilerr // missing/unstattable db file reports 0 size, not an error
	}
	return info.Size(), nil
}

// InitConn is a no-op for SQLite — PRAGMAs are set via DSN parameters.
func (d *SQLiteDialect) InitConn(db *sql.DB) error { return nil }

// SchemaFiles returns the schema files to execute during InitSchema.
func (d *SQLiteDialect) SchemaFiles() []string {
	return []string{"schema.sql"}
}

// CheckpointWAL forces a WAL checkpoint using TRUNCATE mode.
func (d *SQLiteDialect) CheckpointWAL(db *sql.DB) error {
	var busy, log, checkpointed int
	err := db.QueryRowContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed)
	if err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf(
			"WAL checkpoint incomplete: database busy "+
				"(log=%d, checkpointed=%d)", log, checkpointed,
		)
	}
	return nil
}

// SchemaStaleCheck returns the SQL to check whether the most recent migration column exists.
func (d *SQLiteDialect) SchemaStaleCheck() string {
	return "SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'embed_gen'"
}

// IsDuplicateColumnError returns true if the error is "duplicate column name" from ALTER TABLE.
func (d *SQLiteDialect) IsDuplicateColumnError(err error) bool {
	return isSQLiteError(err, "duplicate column name")
}

// IsConflictError returns true if the error is a UNIQUE constraint violation.
func (d *SQLiteDialect) IsConflictError(err error) bool {
	return isSQLiteError(err, "UNIQUE constraint failed")
}

// IsNoSuchTableError returns true if the error indicates a missing table.
func (d *SQLiteDialect) IsNoSuchTableError(err error) bool {
	return isSQLiteError(err, "no such table")
}

// IsNoSuchModuleError returns true if the error indicates a missing module (e.g., fts5).
func (d *SQLiteDialect) IsNoSuchModuleError(err error) bool {
	return isSQLiteError(err, "no such module: fts5")
}

// IsReturningError returns true if the error indicates RETURNING is not supported.
func (d *SQLiteDialect) IsReturningError(err error) bool {
	return isSQLiteError(err, "RETURNING")
}

// BeginExclusive opens a SQLite "BEGIN EXCLUSIVE" transaction on conn.
// In WAL mode this blocks concurrent writers while readers can proceed.
func (d *SQLiteDialect) BeginExclusive(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE")
	return err
}

// BeginWriteSQL returns "BEGIN IMMEDIATE" so the transaction reserves
// the SQLite writer lock at BEGIN, removing the snapshot-isolation race
// that lets two deferred transactions both read the pre-update value.
func (d *SQLiteDialect) BeginWriteSQL() string { return "BEGIN IMMEDIATE" }

// SelectForUpdate returns "" — SQLite has no FOR UPDATE; serialization
// comes from BEGIN IMMEDIATE.
func (d *SQLiteDialect) SelectForUpdate() string { return "" }

// MaintenanceTimeoutResetSQL returns "" — SQLite has no statement_timeout,
// so Store.runMaintenance issues no reset statement and SQLite's
// transactional behavior is unchanged.
func (d *SQLiteDialect) MaintenanceTimeoutResetSQL() string { return "" }

// IsBusyError returns true for SQLITE_BUSY and SQLITE_LOCKED. Matching on
// the result code is more robust than substring matching: BUSY surfaces as
// "database is locked" but LOCKED surfaces as "database table is locked",
// so a single substring cannot catch both.
func (d *SQLiteDialect) IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	var serr sqlite3.Error
	if errors.As(err, &serr) {
		return serr.Code == sqlite3.ErrBusy || serr.Code == sqlite3.ErrLocked
	}
	var serrPtr *sqlite3.Error
	if errors.As(err, &serrPtr) && serrPtr != nil {
		return serrPtr.Code == sqlite3.ErrBusy || serrPtr.Code == sqlite3.ErrLocked
	}
	return false
}

// IsFTSValueTooLargeError always returns false for SQLite: FTS5 has no
// per-value size limit analogous to PostgreSQL's tsvector "string is too long"
// (SQLSTATE 54000), so the backfill never has a row to skip on SQLite.
func (d *SQLiteDialect) IsFTSValueTooLargeError(err error) bool { return false }
