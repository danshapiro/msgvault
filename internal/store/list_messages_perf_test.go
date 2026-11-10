package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

// seedLiveMessages inserts n messages (one source, conversation, participant,
// and a 'from' recipient each) so ListMessages exercises its real joins. Every
// other message is marked deleted_from_source so the live-messages predicate is
// selective, mirroring an archive that retains source-deleted messages.
func seedLiveMessages(tb testing.TB, s *Store, n int) {
	tb.Helper()
	ctx := tb.Context()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'bench@example.com')`)
	require.NoError(tb, err)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
		 VALUES (1, 1, 'conv1', 'email')`)
	require.NoError(tb, err)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO participants (id, email_address, display_name) VALUES (1, 'sender@example.com', 'Sender')`)
	require.NoError(tb, err)

	tx, err := s.db.Begin()
	require.NoError(tb, err)
	msgStmt, err := tx.PrepareContext(tb.Context(), `INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type,
		 sent_at, subject, snippet, size_estimate, deleted_from_source_at)
		VALUES (?, 1, 1, ?, 'email', ?, ?, ?, ?, ?)`)
	require.NoError(tb, err)
	defer func() { _ = msgStmt.Close() }()
	recStmt, err := tx.PrepareContext(tb.Context(), `INSERT INTO message_recipients
		(message_id, participant_id, recipient_type) VALUES (?, 1, 'from')`)
	require.NoError(tb, err)
	defer func() { _ = recStmt.Close() }()

	base := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		sentAt := base.Add(time.Duration(i) * time.Minute)
		var deleted any
		if i%2 == 0 {
			deleted = sentAt
		}
		_, err = msgStmt.ExecContext(tb.Context(), i, fmt.Sprintf("m%d", i), sentAt,
			fmt.Sprintf("Subject %d", i), fmt.Sprintf("snippet %d", i), 1000+i, deleted)
		require.NoError(tb, err)
		_, err = recStmt.ExecContext(tb.Context(), i)
		require.NoError(tb, err)
	}
	require.NoError(tb, tx.Commit())
}

func explainPlan(t *testing.T, s *Store, sql string, args ...any) string {
	t.Helper()
	return dbtest.ExplainQueryPlan(t, s.DB(), sql, args...)
}

// analyzedListDate is an independent fixture ordering oracle. Most dates grow
// with acquisition IDs, but future events, old mail, ties and NULL timestamps
// exercise the same fallback order as a real mixed archive.
func analyzedListDate(id int) time.Time {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	switch id % 97 {
	case 0:
		return base.AddDate(20, 0, 0).Add(time.Duration(id) * time.Minute)
	case 1:
		return base.AddDate(-10, 0, 0).Add(time.Duration(id) * time.Minute)
	case 2, 3:
		return base
	case 4:
		return time.Time{}
	default:
		return base.Add(time.Duration(id) * time.Minute)
	}
}

func seedAnalyzedListSkew(tb testing.TB, s *Store, n int) {
	tb.Helper()
	ctx := tb.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(tb, err)
	defer func() { _ = tx.Rollback() }()
	for id := 1; id <= 10; id++ {
		_, err = tx.ExecContext(ctx, `INSERT INTO sources (id, source_type, identifier)
			VALUES (?, 'gmail', ?)`, id, fmt.Sprintf("archive-%d@example.test", id))
		require.NoError(tb, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO conversations
			(id, source_id, source_conversation_id, conversation_type)
			VALUES (?, ?, ?, 'email')`, id, id, fmt.Sprintf("thread-%d", id))
		require.NoError(tb, err)
	}
	for id, name := range []string{"Sender", "To", "Cc", "Bcc"} {
		_, err = tx.ExecContext(ctx, `INSERT INTO participants
			(id, email_address, display_name) VALUES (?, ?, ?)`,
			id+1, strings.ToLower(name)+"@example.test", name)
		require.NoError(tb, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO labels (id, source_id, source_label_id, name)
		VALUES (1, 1, 'synthetic', 'Synthetic')`)
	require.NoError(tb, err)
	msg, err := tx.PrepareContext(ctx, `INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type,
		sent_at, received_at, internal_date, subject, snippet, size_estimate,
		has_attachments, hidden_text_present, deleted_from_source_at, deleted_at)
		VALUES (?, ?, ?, ?, 'email', ?, ?, ?, ?, ?, ?, 1, 1, ?, ?)`)
	require.NoError(tb, err)
	defer func() { _ = msg.Close() }()
	rec, err := tx.PrepareContext(ctx, `INSERT INTO message_recipients
		(message_id, participant_id, recipient_type, email_address) VALUES (?, ?, ?, ?)`)
	require.NoError(tb, err)
	defer func() { _ = rec.Close() }()
	label, err := tx.PrepareContext(ctx, `INSERT INTO message_labels (message_id, label_id) VALUES (?, 1)`)
	require.NoError(tb, err)
	defer func() { _ = label.Close() }()
	for id := 1; id <= n; id++ {
		stamp := analyzedListDate(id)
		var sent, received, internal, sourceDeleted, deleted any
		if !stamp.IsZero() {
			switch id % 3 {
			case 0:
				received = stamp
			case 1:
				internal = stamp
			default:
				sent = stamp
			}
		}
		if id%40 == 0 {
			sourceDeleted = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id%1600) * time.Minute)
		}
		if id%40 == 1 {
			deleted = time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id%1600) * time.Minute)
		}
		source := 1 + id%10
		_, err = msg.ExecContext(ctx, id, source, source, fmt.Sprintf("message-%d", id),
			sent, received, internal, fmt.Sprintf("Subject %d", id), fmt.Sprintf("Snippet %d", id),
			1000+id, sourceDeleted, deleted)
		require.NoError(tb, err)
		for i, kind := range []string{"from", "to", "cc", "bcc"} {
			address := kind + "@example.test"
			if kind == "from" {
				// Diverse archived envelope aliases retain one known participant.
				// A constant NULL envelope would make the sender-only partial
				// index an unrelated, misleading skip-scan/sort candidate.
				address = fmt.Sprintf("sender-%d@example.test", id)
			}
			_, err = rec.ExecContext(ctx, id, i+1, kind, address)
			require.NoError(tb, err)
		}
		_, err = label.ExecContext(ctx, id)
		require.NoError(tb, err)
	}
	require.NoError(tb, tx.Commit())
}

func analyzedListIDs(n int, requested, authorized []int64) []int64 {
	var ids []int64
	for id := 1; id <= n; id++ {
		source := int64(1 + id%10)
		if id%40 == 0 || id%40 == 1 ||
			(requested != nil && !slices.Contains(requested, source)) ||
			(authorized != nil && !slices.Contains(authorized, source)) {
			continue
		}
		ids = append(ids, int64(id))
	}
	slices.SortFunc(ids, func(a, b int64) int {
		if dateOrder := analyzedListDate(int(b)).Compare(analyzedListDate(int(a))); dateOrder != 0 {
			return dateOrder
		}
		return int(b - a)
	})
	return ids
}

func captureListQueries(tb testing.TB, s *Store, read func()) (string, string) {
	tb.Helper()
	original := s.db.rebind
	var mu sync.Mutex
	var statements []string
	s.db.rebind = func(query string) string {
		bound := original(query)
		mu.Lock()
		statements = append(statements, bound)
		mu.Unlock()
		return bound
	}
	defer func() { s.db.rebind = original }()
	read()
	mu.Lock()
	defer mu.Unlock()
	var counts, pages []string
	for _, query := range statements {
		normalized := strings.ToUpper(query)
		if strings.Contains(normalized, "SELECT COUNT(*) FROM") {
			counts = append(counts, query)
		}
		if strings.Contains(normalized, "LIMIT ? OFFSET ?") {
			pages = append(pages, query)
		}
	}
	require.Len(tb, counts, 1)
	require.Len(tb, pages, 1)
	return counts[0], pages[0]
}

func openAnalyzedList(tb testing.TB, n int) (*Store, string) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "analyzed-list.db")
	s, err := OpenForTest(tb.Context(), path)
	require.NoError(tb, err)
	require.NoError(tb, s.InitSchema())
	seedAnalyzedListSkew(tb, s, n)
	// InitSchema installs a 1000-row analysis limit on serving connections.
	// A full refresh must select its own connection and clear that inherited
	// sample bound, just as scheduled full message analysis does.
	analysis, err := s.DB().Conn(tb.Context())
	require.NoError(tb, err)
	_, err = analysis.ExecContext(tb.Context(), "PRAGMA analysis_limit=0")
	require.NoError(tb, err)
	_, err = analysis.ExecContext(tb.Context(), "ANALYZE")
	require.NoError(tb, err)
	_, err = analysis.ExecContext(tb.Context(), "PRAGMA optimize")
	require.NoError(tb, err)
	var persistedStat string
	require.NoError(tb, analysis.QueryRowContext(tb.Context(),
		"SELECT stat FROM sqlite_stat1 WHERE idx='idx_messages_live_sent_at'").Scan(&persistedStat))
	require.NoError(tb, analysis.Close())
	require.NoError(tb, s.Close())
	s, err = OpenForTest(tb.Context(), path)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, s.Close()) })
	require.NoError(tb, s.InitSchema())

	// InitSchema prewarms the pool. Discard those idle connections so these
	// checks prove genuinely new physical connections load the same statistics.
	s.DB().SetMaxIdleConns(0)
	defer setSQLitePoolSize(s.DB())
	require.Zero(tb, s.DB().Stats().OpenConnections)
	// Hold a connection while acquiring another to prove both see the
	// persisted full statistics and mandatory index.
	first, err := s.DB().Conn(tb.Context())
	require.NoError(tb, err)
	defer func() { _ = first.Close() }()
	second, err := s.DB().Conn(tb.Context())
	require.NoError(tb, err)
	defer func() { _ = second.Close() }()
	var firstStat, secondStat, index string
	require.NoError(tb, first.QueryRowContext(tb.Context(),
		"SELECT stat FROM sqlite_stat1 WHERE idx='idx_messages_live_sent_at'").Scan(&firstStat))
	require.NoError(tb, second.QueryRowContext(tb.Context(),
		"SELECT stat FROM sqlite_stat1 WHERE idx='idx_messages_live_sent_at'").Scan(&secondStat))
	assert.Equal(tb, firstStat, secondStat)
	assert.Equal(tb, persistedStat, firstStat)
	require.NoError(tb, second.QueryRowContext(tb.Context(),
		"SELECT name FROM sqlite_master WHERE name='idx_messages_live_sent_at'").Scan(&index))
	assert.Equal(tb, "idx_messages_live_sent_at", index)
	return s, path
}

func TestListMessages_AnalyzedFullQuery(t *testing.T) {
	for _, n := range []int{80000, 160000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			assert := assert.New(t)
			s, path := openAnalyzedList(t, n)
			var messages []APIMessage
			var total int64
			countSQL, pageSQL := captureListQueries(t, s, func() {
				var err error
				messages, total, err = s.ListMessagesContext(t.Context(), 40, 20, nil)
				require.NoError(t, err)
			})
			require.Len(t, messages, 20)
			assert.Equal(int64(n*38/40), total)
			ids := analyzedListIDs(n, nil, nil)
			for i, message := range messages {
				id := ids[40+i]
				assert.Equal(id, message.ID)
				assert.Equal(int64(1+id%10), message.SourceID)
				assert.Equal(fmt.Sprintf("thread-%d", 1+id%10), message.SourceConversationID)
				assert.Equal(fmt.Sprintf("message-%d", id), message.SourceMessageID)
				assert.Equal(fmt.Sprintf("Subject %d", id), message.Subject)
				assert.Equal(fmt.Sprintf("Snippet %d", id), message.Snippet)
				assert.True(analyzedListDate(int(id)).Equal(message.SentAt))
				assert.Equal("Sender <sender@example.test>", message.From)
				assert.Equal("sender@example.test", message.FromEmail)
				assert.Equal("Sender", message.FromName)
				assert.Equal([]string{"To <to@example.test>"}, message.To)
				assert.Equal([]string{"Cc <cc@example.test>"}, message.Cc)
				assert.Equal([]string{"Bcc <bcc@example.test>"}, message.Bcc)
				assert.Equal([]string{"Synthetic"}, message.Labels)
				assert.True(message.HasAttachments)
				assert.True(message.HiddenTextPresent)
				assert.Equal(int64(1000)+id, message.SizeEstimate)
			}
			countPlan := explainPlan(t, s, countSQL)
			pagePlan := explainPlan(t, s, pageSQL, 20, 40)
			t.Logf("count plan:\n%s\nfull page plan:\n%s", countPlan, pagePlan)
			assert.Contains(countPlan, "idx_messages_live_sent_at")
			assert.Contains(pagePlan, "idx_messages_live_sent_at")
			assert.NotContains(pagePlan, "TEMP B-TREE")
			measureListCLI(t, s, path, n, "count", countSQL)
			measureListCLI(t, s, path, n, "page", pageSQL, 20, 40)
			measureListCLI(t, s, path, n, "deep-page", pageSQL, 20, n/2)
			measureListTrim(t, s, path, n)
		})
	}
}

var trimDriverSequence atomic.Int64

// newListTrimDB uses a supported connection hook only on a test-owned pool.
// SQLite's default trim removes U+0020 only, and propagates NULL.
func newListTrimDB(t testing.TB, path string, calls *atomic.Int64) *sql.DB {
	t.Helper()
	driver := fmt.Sprintf("list-trim-%d", trimDriverSequence.Add(1))
	sql.Register(driver, &sqlite3.SQLiteDriver{ConnectHook: func(conn *sqlite3.SQLiteConn) error {
		if err := sqliteutil.RegisterFunctions(conn); err != nil {
			return err
		}
		return conn.RegisterFunc("trim", func(value any) any {
			calls.Add(1)
			switch value := value.(type) {
			case nil:
				return nil
			case []byte:
				if value == nil {
					return nil
				}
				return strings.Trim(string(value), " ")
			case string:
				return strings.Trim(value, " ")
			default:
				return strings.Trim(fmt.Sprint(value), " ")
			}
		}, true)
	}})
	db, err := sql.Open(driver, appendSQLiteParams(path, testSQLiteParams))
	require.NoError(t, err)
	setSQLitePoolSize(db)
	return db
}

// The full analyzed pre-repair query exceeds the generous metadata bound at
// both fixture sizes; a sampled-plan candidate that did not was rejected.
func measureListTrim(t *testing.T, s *Store, path string, n int) {
	t.Helper()
	var calls atomic.Int64
	db := newListTrimDB(t, path, &calls)
	defer func() { require.NoError(t, db.Close()) }()
	for _, value := range []any{nil, "", "   ", " Sender ", "\tSender\n", "\u00a0Sender\u00a0"} {
		var builtin, instrumented sql.NullString
		require.NoError(t, s.DB().QueryRowContext(t.Context(), "SELECT trim(?)", value).Scan(&builtin))
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT trim(?)", value).Scan(&instrumented))
		assert.Equal(t, builtin, instrumented, "transparent SQLite U+0020/NULL semantics")
	}
	original := s.db.DB
	s.db.DB = db
	defer func() { s.db.DB = original }()
	calls.Store(0)
	messages, _, err := s.ListMessagesContext(t.Context(), 40, 20, nil)
	require.NoError(t, err)
	require.Len(t, messages, 20)
	t.Logf("trim metadata work n=%d evaluations=%d", n, calls.Load())
	assert.LessOrEqual(t, calls.Load(), int64(100*(40+20)), "metadata evaluation must remain bounded by offset+limit")
}

func TestListMessages_TrimProjectionSemantics(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "trim.db")
	s, err := OpenForTest(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(s.Close()) })
	require.NoError(s.InitSchema())
	source, err := s.GetOrCreateSourceContext(t.Context(), "test", "projection@example.test")
	require.NoError(err)
	conversation, err := s.EnsureConversation(t.Context(), source.ID, "projection", "Synthetic")
	require.NoError(err)
	_, err = s.DB().ExecContext(t.Context(), `INSERT INTO participants (id, email_address, display_name) VALUES
		(1, 'null@example.test', NULL), (2, 'empty@example.test', ''),
		(3, NULL, NULL), (4, 'space@example.test', char(9) || ' Name ' || char(10))`)
	require.NoError(err)
	for id := 1; id <= 5; id++ {
		message := seedMessage(t, s, source.ID, conversation, strconv.Itoa(id), "Synthetic", "")
		if id <= 4 {
			require.NoError(s.ReplaceMessageRecipientsContext(t.Context(), message, "from", []int64{int64(id)}, nil))
		}
	}
	want, wantTotal, err := s.ListMessagesContext(t.Context(), 0, 10, nil)
	require.NoError(err)
	require.Len(want, 5)
	assert.Equal("", want[0].From, "missing sender")
	assert.Equal("\t Name \n <space@example.test>", want[1].From, "tabs/newlines survive SQLite trim")
	assert.Equal("", want[2].From, "NULL sender email/name")
	assert.Equal("empty@example.test", want[3].From)
	assert.Equal("null@example.test", want[4].From)
	var calls atomic.Int64
	db := newListTrimDB(t, path, &calls)
	defer func() { require.NoError(db.Close()) }()
	original := s.db.DB
	s.db.DB = db
	defer func() { s.db.DB = original }()
	got, gotTotal, err := s.ListMessagesContext(t.Context(), 0, 10, nil)
	require.NoError(err)
	assert.Equal(wantTotal, gotTotal)
	assert.Equal(want, got, "full real sender projections are unchanged by the transparent hook")
}

func measureListCLI(t *testing.T, s *Store, path string, n int, kind, query string, args ...int) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil {
		t.Logf("SQLite CLI counters unavailable: %v; exact Go-driver query plans still checked", err)
		return
	}
	for _, arg := range args {
		query = strings.Replace(query, "?", strconv.Itoa(arg), 1)
	}
	require.NotContains(t, query, "?")
	var goVersion string
	require.NoError(t, s.DB().QueryRowContext(t.Context(), "SELECT sqlite_version()").Scan(&goVersion))
	start := time.Now()
	out, err := exec.CommandContext(t.Context(), "/usr/bin/sqlite3", "-readonly", "-stats", path, query).CombinedOutput()
	require.NoError(t, err, "%s", out)
	elapsed := time.Since(start)
	version, err := exec.CommandContext(t.Context(), "/usr/bin/sqlite3", "-readonly", path, "SELECT sqlite_version()").Output()
	require.NoError(t, err)
	explain, err := exec.CommandContext(t.Context(), "/usr/bin/sqlite3", "-readonly", path, "EXPLAIN "+query).CombinedOutput()
	require.NoError(t, err, "%s", explain)
	t.Logf("%s n=%d Go SQLite=%s CLI SQLite=%s elapsed=%s\n%s", kind, n, goVersion, strings.TrimSpace(string(version)), elapsed, out)
	if kind != "count" && strings.Contains(query, "INDEXED BY idx_messages_live_sent_at") {
		metrics := regexp.MustCompile(`(?m)^(Virtual Machine Steps|Fullscan Steps|Sort Operations):\s+(\d+)`).FindAllStringSubmatch(string(out), -1)
		require.Len(t, metrics, 3)
		for _, metric := range metrics {
			value, err := strconv.Atoi(metric[2])
			require.NoError(t, err)
			bound := 100 * (args[0] + args[1] + 1)
			switch metric[1] {
			case "Sort Operations":
				assert.Zero(t, value)
			case "Fullscan Steps":
				assert.LessOrEqual(t, value, args[0]+args[1])
			default:
				assert.Less(t, value, bound)
			}
		}
	}
	if dir := os.Getenv("MSGVAULT_LIST_EVIDENCE_DIR"); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0755))
		prefix := filepath.Join(dir, fmt.Sprintf("%d-%s", n, kind))
		require.NoError(t, os.WriteFile(prefix+".sql", []byte(query), 0644))
		require.NoError(t, os.WriteFile(prefix+".txt", []byte(fmt.Sprintf("Go SQLite=%s CLI SQLite=%s elapsed=%s\n%s\nEXPLAIN\n%s", goVersion, version, elapsed, out, explain)), 0644))
	}
}

func TestListMessages_AnalyzedPagesAndScopes(t *testing.T) {
	const n = 80000
	s, _ := openAnalyzedList(t, n)
	cases := []struct {
		name                  string
		requested, authorized []int64
		scoped                bool
	}{
		{name: "unrestricted"},
		{name: "requested", requested: []int64{1, 3}, scoped: true},
		{name: "empty-request", requested: []int64{}, scoped: true},
		{name: "authorized", authorized: []int64{1, 2}, scoped: true},
		{name: "empty-authorization", authorized: []int64{}, scoped: true},
		{name: "intersection", requested: []int64{1, 3}, authorized: []int64{1, 2}, scoped: true},
		{name: "disjoint", requested: []int64{3}, authorized: []int64{1, 2}, scoped: true},
		{name: "authorized-empty-request", requested: []int64{}, authorized: []int64{1}, scoped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			ctx := t.Context()
			if tc.authorized != nil {
				ctx = sourcescope.WithSourceIDs(ctx, tc.authorized)
			}
			want := analyzedListIDs(n, tc.requested, tc.authorized)
			for _, offset := range []int{0, 40, n / 2, max(0, len(want)-7), len(want), len(want) + 20} {
				var messages []APIMessage
				var total int64
				count, page := captureListQueries(t, s, func() {
					var err error
					messages, total, err = s.ListMessagesContext(ctx, offset, 20, tc.requested)
					require.NoError(t, err)
				})
				assert.Equal(int64(len(want)), total)
				var got []int64
				for _, message := range messages {
					got = append(got, message.ID)
				}
				start := min(offset, len(want))
				expected := want[start:min(start+20, len(want))]
				assert.Equal(len(expected), len(got))
				assert.Equal(append([]int64{}, expected...), append([]int64{}, got...))
				if tc.scoped {
					assert.NotContains(count, "INDEXED BY")
					assert.NotContains(page, "INDEXED BY")
				}
			}
		})
	}
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	_, _, err := s.ListMessagesContext(ctx, 0, 20, nil)
	require.ErrorIs(err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second)
	messages, _, err := s.ListMessagesContext(t.Context(), 0, 20, nil)
	require.NoError(err)
	require.Len(messages, 20)
}

func TestListMessages_LiveIndexSetup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "index-setup.db")
	s, err := OpenForTest(t.Context(), path)
	require.NoError(err)
	require.NoError(s.InitSchema())
	var name string
	require.NoError(s.DB().QueryRowContext(t.Context(),
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_messages_live_sent_at'").Scan(&name))
	assert.Equal("idx_messages_live_sent_at", name)
	_, err = s.DB().ExecContext(t.Context(), "DROP INDEX idx_messages_live_sent_at")
	require.NoError(err)
	_, _, err = s.ListMessagesContext(t.Context(), 0, 20, nil)
	require.ErrorContains(err, "idx_messages_live_sent_at", "missing required setup remains an explicit error")
	_, err = s.DB().ExecContext(t.Context(),
		"UPDATE archive_metadata SET value=? WHERE key='schema_version'", strconv.Itoa(SchemaVersion-1))
	require.NoError(err)
	require.NoError(s.Close())

	// InitSchema also installs the index for an older archive missing it.
	s, err = OpenForTest(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(s.Close()) })
	require.NoError(s.InitSchema())
	version, err := s.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(SchemaVersion, version)
	messages, total, err := s.ListMessagesContext(t.Context(), 0, 20, nil)
	require.NoError(err)
	assert.Empty(messages)
	assert.Zero(total)
	require.NoError(s.DB().QueryRowContext(t.Context(),
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_messages_live_sent_at'").Scan(&name))
	assert.Equal("idx_messages_live_sent_at", name)
}

func TestListMessages_PostgreSQLQueryRemainsPortable(t *testing.T) {
	s := openTestStore(t)
	// Exercise the PostgreSQL dialect guard with the actual generated SQL on
	// an empty SQLite store. This is SQL-generation coverage, not a substitute
	// for the separately configured PostgreSQL integration suite.
	dialect := s.dialect
	s.dialect = &PostgreSQLDialect{}
	defer func() { s.dialect = dialect }()
	count, page := captureListQueries(t, s, func() {
		messages, total, err := s.ListMessagesContext(t.Context(), 0, 20, nil)
		require.NoError(t, err)
		assert.Empty(t, messages)
		assert.Zero(t, total)
	})
	assert.NotContains(t, count, "INDEXED BY")
	assert.NotContains(t, page, "INDEXED BY")
}

func BenchmarkListMessagesAnalyzed(b *testing.B) {
	for _, n := range []int{80000, 160000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s, _ := openAnalyzedList(b, n)
			var messages []APIMessage
			count, page := captureListQueries(b, s, func() {
				var err error
				messages, _, err = s.ListMessagesContext(b.Context(), 40, 20, nil)
				require.NoError(b, err)
			})
			ids := make([]int64, len(messages))
			for i, message := range messages {
				ids[i] = message.ID
			}
			b.Run("count", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					var total int64
					require.NoError(b, s.DB().QueryRowContext(b.Context(), count).Scan(&total))
					require.Equal(b, int64(n*38/40), total)
				}
			})
			b.Run("page", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					rows, err := s.db.QueryContext(b.Context(), page, 20, 40)
					require.NoError(b, err)
					got, _, err := scanMessageRows(rows)
					require.NoError(b, rows.Close())
					require.NoError(b, err)
					require.Len(b, got, 20)
				}
			})
			b.Run("hydration", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					require.NoError(b, s.batchPopulateContext(b.Context(), messages, ids))
				}
			})
			b.Run("endpoint", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					messages, total, err := s.ListMessagesContext(b.Context(), 40, 20, nil)
					require.NoError(b, err)
					require.Len(b, messages, 20)
					require.Equal(b, int64(n*38/40), total)
				}
			})
		})
	}
}

// TestListMessages_UsesLiveIndex verifies the ListMessages COUNT and page
// queries are served by partial live-message indexes instead of a full table
// scan + temp-B-tree sort. This locks in the perf fix for GET /api/v1/messages,
// which was seconds per page on a multi-GB SQLite archive.
func TestListMessages_UsesLiveIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	s, err := OpenForTest(t.Context(), filepath.Join(dir, "list.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	seedLiveMessages(t, s, 2000)

	// The index InitSchema created must exist.
	var idxCount int
	require.NoError(s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_live_sent_at'`,
	).Scan(&idxCount))
	assert.Equal(1, idxCount, "idx_messages_live_sent_at should be created by InitSchema")

	countSQL := "SELECT COUNT(*) FROM messages WHERE " + LiveMessagesWhere("", true)
	countPlan := explainPlan(t, s, countSQL)
	assert.Contains(countPlan, "USING INDEX idx_messages_live_",
		"COUNT should use the partial index, not a full scan:\n%s", countPlan)

	pageSQL := fmt.Sprintf(`SELECT m.id FROM messages m
		WHERE %s
		ORDER BY COALESCE(m.sent_at, m.received_at, m.internal_date) DESC, m.id DESC
		LIMIT 20 OFFSET 0`, LiveMessagesWhere("m", true))
	pagePlan := explainPlan(t, s, pageSQL)
	assert.Contains(pagePlan, "idx_messages_live_sent_at",
		"page query should use the partial index:\n%s", pagePlan)
	assert.NotContains(pagePlan, "TEMP B-TREE",
		"page query should walk the index in order, not sort:\n%s", pagePlan)

	// The public API path returns the right rows: live messages only,
	// newest first, with the total excluding source-deleted rows.
	msgs, total, err := s.ListMessages(0, 20)
	require.NoError(err)
	assert.Equal(int64(1000), total, "1000 of 2000 messages are live")
	require.Len(msgs, 20)
	assert.Equal(int64(1999), msgs[0].ID, "newest live message id (2000 is deleted)")
	assert.Equal(int64(1997), msgs[1].ID)
}

// TestSearchMessages_DateBoundsUseInstantIndex verifies that timezone-correct
// date predicates can seek into a partial expression index. Without the
// matching index, julianday(COALESCE(...)) forces date-filtered result counts
// to scan every live message in large SQLite archives.
func TestSearchMessages_DateBoundsUseInstantIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	s, err := OpenForTest(t.Context(), filepath.Join(t.TempDir(), "search-date.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	seedLiveMessages(t, s, 2000)

	const indexName = "idx_messages_live_timestamp_julianday"
	var idxCount int
	require.NoError(s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, indexName,
	).Scan(&idxCount))
	assert.Equal(1, idxCount, "%s should be created by InitSchema", indexName)

	countSQL := `SELECT COUNT(*) FROM messages m
		WHERE ` + LiveMessagesWhere("m", true) + `
		AND julianday(COALESCE(m.sent_at, m.received_at, m.internal_date)) >= julianday(?)`
	countPlan := explainPlan(t, s, countSQL, "2015-01-01T16:00:00-05:00")
	assert.Contains(countPlan, indexName,
		"date-filtered COUNT should seek the instant index:\n%s", countPlan)
}

func TestConversationWindow_UsesConversationTimelineIndex(t *testing.T) {
	requirements := require.New(t)
	s, err := OpenForTest(t.Context(), filepath.Join(t.TempDir(), "window-plan.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchema())
	seedLiveMessages(t, s, 2000)
	start := time.Date(2015, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, dated := range []bool{false, true} {
		for _, bound := range []*time.Time{nil, &start} {
			if !dated && bound != nil {
				continue // Bounded windows reject undated anchors before querying the window.
			}
			t.Run(fmt.Sprintf("dated%t/bounded%t", dated, bound != nil), func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				var timestamp any
				if dated {
					timestamp = start.Add(940 * time.Minute)
				}
				_, err := s.db.ExecContext(t.Context(), `UPDATE messages SET sent_at = ? WHERE id = 1000`, timestamp)
				requirements.NoError(err)

				// Capture the actual count and hydration statements, including the
				// segment wrappers, unions, and outer ordering used by the reader.
				var countSQL, hydrateSQL string
				rebind := s.db.rebind
				s.db.rebind = func(query string) string {
					if strings.HasPrefix(query, "SELECT (SELECT COUNT(*)") {
						countSQL = query
					}
					if strings.Contains(query, "WITH selected AS") {
						hydrateSQL = query
					}
					return rebind(query)
				}
				t.Cleanup(func() { s.db.rebind = rebind })
				_, err = s.GetConversationWindowContext(t.Context(), 1, 1000, 25, 25, bound, nil)
				requirements.NoError(err)
				s.db.rebind = rebind
				requirements.NotEmpty(countSQL)
				requirements.NotEmpty(hydrateSQL)

				before := s.conversationWindowSegments(t.Context(), 1, 1000, dated, true, bound, nil)
				after := s.conversationWindowSegments(t.Context(), 1, 1000, dated, false, bound, nil)
				_, beforeArgs := conversationSegmentCounts(before)
				_, afterArgs := conversationSegmentCounts(after)
				countPlan := explainPlan(t, s, countSQL, append(beforeArgs, afterArgs...)...)
				assertions.Contains(countPlan, "SEARCH m USING INDEX idx_messages_conversation_timeline")
				assertions.NotContains(countPlan, "SCAN m")
				assertions.NotContains(countPlan, "TEMP B-TREE")

				_, beforeArgs = conversationSegmentSelection(before, 25, "DESC")
				_, afterArgs = conversationSegmentSelection(after, 25, "ASC")
				beforeArgs = append(beforeArgs, int64(1000))
				beforeArgs = append(beforeArgs, afterArgs...)
				plan := explainPlan(t, s, hydrateSQL, beforeArgs...)
				assertions.Contains(plan, "SEARCH m USING INDEX idx_messages_conversation_timeline")
				assertions.NotContains(plan, "SCAN m")
			})
		}
	}
}
