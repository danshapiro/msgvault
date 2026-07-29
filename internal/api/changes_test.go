package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// newChangesServer builds a server over a real store on whichever backend the
// test run targets, so the feed is exercised through the same router, handler,
// and SQL a client would reach.
func newChangesServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})
	return srv, st
}

// seedChangedMessages inserts count messages through the same UpsertMessage
// path importers use, so the INSERT trigger stamps content_changed_at exactly
// as it would in production. Ids come back in insert order.
func seedChangedMessages(t *testing.T, st *store.Store, count int) []int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "changes@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(
		src.ID, "changes-conv", "email_thread", "Changes thread")
	require.NoError(t, err, "EnsureConversationWithType")

	ids := make([]int64, 0, count)
	for i := 1; i <= count; i++ {
		id, err := st.UpsertMessage(&store.Message{
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("changes-msg-%d", i),
			ConversationID:  convID,
			MessageType:     "email",
			Subject:         sql.NullString{String: fmt.Sprintf("changes subject %d", i), Valid: true},
			Snippet:         sql.NullString{String: fmt.Sprintf("changes snippet %d", i), Valid: true},
			SizeEstimate:    int64(1000 + i),
		})
		require.NoError(t, err, "UpsertMessage")
		ids = append(ids, id)
	}
	return ids
}

// seedMoreChangedMessages inserts count further messages through UpsertMessage,
// under a source_message_id prefix of the caller's choosing so a second batch
// creates new rows instead of re-upserting the first one (an identical upsert
// is value-guarded and would not move any watermark).
func seedMoreChangedMessages(t *testing.T, st *store.Store, prefix string, count int) []int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "changes@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(
		src.ID, "changes-conv", "email_thread", "Changes thread")
	require.NoError(t, err, "EnsureConversationWithType")

	ids := make([]int64, 0, count)
	for i := 1; i <= count; i++ {
		id, err := st.UpsertMessage(&store.Message{
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("%s-%d", prefix, i),
			ConversationID:  convID,
			MessageType:     "email",
			Subject:         sql.NullString{String: fmt.Sprintf("%s subject %d", prefix, i), Valid: true},
			SizeEstimate:    int64(2000 + i),
		})
		require.NoError(t, err, "UpsertMessage")
		ids = append(ids, id)
	}
	return ids
}

// setChangesWatermark forces content_changed_at to an exact value. The
// statement names only content_changed_at, so no UPDATE OF list matches and the
// trigger does not overwrite it.
func setChangesWatermark(t *testing.T, st *store.Store, value string, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		_, err := st.DB().Exec(
			st.Rebind(`UPDATE messages SET content_changed_at = ? WHERE id = ?`), value, id)
		require.NoErrorf(t, err, "set content_changed_at for message %d", id)
	}
}

// setChangesMessageTimestamp writes a lifecycle timestamp column directly.
// These are content columns, so the write also bumps the watermark.
func setChangesMessageTimestamp(t *testing.T, st *store.Store, id int64, col string, value time.Time) {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind(fmt.Sprintf(`UPDATE messages SET %s = ? WHERE id = ?`, col)), value, id)
	require.NoErrorf(t, err, "set messages.%s for message %d", col, id)
}

// subSecondWatermark is a watermark literal carrying the finest sub-second
// resolution the target backend actually produces: microseconds on PostgreSQL,
// milliseconds on SQLite (strftime('%f') stops there, and the SQLite cursor
// parameter is encoded at that same resolution). Either way the fraction is
// non-zero, which is what the RFC3339Nano serialisation has to preserve.
func subSecondWatermark(st *store.Store) string {
	if st.IsPostgreSQL() {
		return "2026-07-26 10:00:00.731123"
	}
	return "2026-07-26 10:00:00.731"
}

// changesTarget builds a /messages/changes URL. A zero value for since,
// sinceID, or limit omits that parameter, which is how a first-run consumer
// calls the feed.
func changesTarget(since string, sinceID int64, limit int) string {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if sinceID != 0 {
		q.Set("since_id", strconv.FormatInt(sinceID, 10))
	}
	if limit != 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	target := "/api/v1/messages/changes"
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}

// changesFarFuture is a cursor no watermark can reach, so a page requested with
// it comes back empty and carries nothing but the clock reading.
const changesFarFuture = "2999-01-01T00:00:00Z"

// changesServerTime reads the database clock the way a client does: every page
// carries it, empty ones included.
func changesServerTime(t *testing.T, srv *Server) time.Time {
	t.Helper()
	resp := getChangesPage(t, srv, changesTarget(changesFarFuture, 0, 1))
	at, err := time.Parse(time.RFC3339Nano, resp.ServerTime)
	require.NoErrorf(t, err, "server_time %q must parse as RFC3339", resp.ServerTime)
	return at
}

// changesCompleteThrough reads how far the feed is complete — its page bound,
// which is what decides whether a row is publishable yet.
func changesCompleteThrough(t *testing.T, srv *Server) time.Time {
	t.Helper()
	resp := getChangesPage(t, srv, changesTarget(changesFarFuture, 0, 1))
	at, err := time.Parse(time.RFC3339Nano, resp.CompleteThrough)
	require.NoErrorf(t, err, "complete_through %q must parse as RFC3339", resp.CompleteThrough)
	return at
}

// settleChangesClock blocks until every watermark written so far is
// publishable. The feed withholds the instant it is bounded at — that instant
// can still receive commits — so a test that seeds rows through the triggers
// and then expects to see them has to let the bound leave it. Tests that stamp
// their own watermarks in the past do not need it.
func settleChangesClock(t *testing.T, srv *Server) {
	t.Helper()
	start := changesServerTime(t, srv)
	deadline := time.Now().Add(30 * time.Second)
	for !changesCompleteThrough(t, srv).After(start) {
		if time.Now().After(deadline) {
			require.Failf(t, "the change feed stopped advancing",
				"complete_through never moved past %s; something is holding a write "+
					"transaction open", start)
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// getChangesPage serves one feed request and decodes a successful response.
func getChangesPage(t *testing.T, srv *Server, target string) ChangesResponse {
	t.Helper()
	w := doGet(srv, target)
	require.Equalf(t, http.StatusOK, w.Code, "GET %s: %s", target, w.Body.String())
	var resp ChangesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode changes response")
	return resp
}

// changedIDs extracts the ids of a page in the order the feed returned them.
func changedIDs(resp ChangesResponse) []int64 {
	ids := make([]int64, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestChangesEndpoint_WalksEveryMessageExactlyOnce follows next_since/
// next_since_id the way a client would, across a block sharing one watermark.
// The final page is also the exact-boundary case: 25 rows walked five at a time
// ends on a full page with nothing after it, and has_more must say so.
func TestChangesEndpoint_WalksEveryMessageExactlyOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	const total = 25
	const pageSize = 5
	want := seedChangedMessages(t, st, total)
	setChangesWatermark(t, st, subSecondWatermark(st), want...)

	var (
		got     []int64
		since   string
		sinceID int64
		pages   int
	)
	for {
		require.Lessf(pages, 200,
			"the feed did not terminate after %d pages: the cursor is not advancing", pages)
		resp := getChangesPage(t, srv, changesTarget(since, sinceID, pageSize))
		require.LessOrEqual(len(resp.Messages), pageSize, "a page must not exceed the limit")
		require.Equal(len(resp.Messages), resp.Count, "count must describe the page it ships with")
		if len(resp.Messages) == 0 {
			assert.False(resp.HasMore, "an empty page has nothing after it")
			break
		}
		pages++
		got = append(got, changedIDs(resp)...)
		assert.Equalf(len(got) < total, resp.HasMore,
			"has_more after %d of %d rows", len(got), total)
		since, sinceID = resp.NextSince, resp.NextSinceID
	}

	require.Len(got, total,
		"a walk over %d rows sharing one watermark returned %d rows: the HTTP cursor "+
			"must deliver each row exactly once", total, len(got))
	assert.Equal(want, got,
		"the walk must return every same-instant row exactly once, in id order")
	assert.Equal(total/pageSize, pages, "walk page count")
}

// TestChangesEndpoint_CursorRoundTripsFullPrecision is the loop guard: take
// next_since from one response, send it back, and assert the second page does
// not repeat the first. A cursor serialised with time.RFC3339 loses the
// sub-second part of the watermark, and the truncated value re-selects the page
// that was just delivered — forever.
func TestChangesEndpoint_CursorRoundTripsFullPrecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	watermark := subSecondWatermark(st)
	setChangesWatermark(t, st, watermark, ids...)

	first := getChangesPage(t, srv, changesTarget("", 0, 10))
	require.Equal(ids, changedIDs(first), "the first page returns the whole archive")

	last := first.Messages[len(first.Messages)-1]
	require.Equal(last.ContentChangedAt, first.NextSince,
		"next_since must be the last row's watermark verbatim")
	require.Equal(last.ID, first.NextSinceID, "next_since_id must be the last row's id")

	cursor, err := time.Parse(time.RFC3339Nano, first.NextSince)
	require.NoErrorf(err, "next_since %q must parse as RFC3339", first.NextSince)
	require.NotZerof(cursor.Nanosecond(),
		"this test is meaningless unless the watermark %q carries a sub-second part; "+
			"next_since was %q", watermark, first.NextSince)
	assert.NotEqual(cursor.Truncate(time.Second).Format(time.RFC3339), first.NextSince,
		"next_since must not be truncated to whole seconds")

	second := getChangesPage(t, srv, changesTarget(first.NextSince, first.NextSinceID, 10))
	assert.Empty(second.Messages,
		"resending next_since must not redeliver the page it came from; a "+
			"second-truncated cursor makes a polling consumer loop forever")
}

// TestChangesEndpoint_ResumeFromEarlierCursorRedelivers documents the
// consequence of the overlap advice: resuming from an earlier cursor
// re-delivers rows rather than erroring or skipping. That makes an overlapping
// re-read safe. It says nothing about whether every change reaches the feed —
// a change committing after a later transaction's watermark was already
// returned is missed, and no cursor arithmetic here can recover it.
func TestChangesEndpoint_ResumeFromEarlierCursorRedelivers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 6)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	first := getChangesPage(t, srv, changesTarget("", 0, 3))
	require.Len(first.Messages, 3, "first page")
	require.True(first.HasMore, "more rows remain")

	// Resume from the position the consumer held after the FIRST row of the
	// first page: the two rows it already saw come back again.
	resumed := first.Messages[0]
	rewound := getChangesPage(t, srv, changesTarget(resumed.ContentChangedAt, resumed.ID, 10))
	assert.Equal(ids[1:], changedIDs(rewound),
		"a cursor resumed from an earlier position redelivers the rows after it, "+
			"so a consumer may safely re-read an overlapping window")
}

// TestChangesEndpoint_EmptyArchiveReturnsEchoableCursor covers the first poll
// of an archive with nothing in it. The response must be directly re-sendable
// as the next request, and server_time must be a real database clock reading:
// the store returns a zero ServerTime for a non-positive limit, so a handler
// that forwarded its raw limit would publish "0001-01-01T00:00:00Z" here.
func TestChangesEndpoint_EmptyArchiveReturnsEchoableCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _ := newChangesServer(t)

	w := doGet(srv, changesTarget("", 0, 0))
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(w.Body.Bytes(), &raw), "decode response object")
	assert.JSONEq("[]", string(raw["messages"]),
		"messages must be an empty array, never null: a client that ranges over "+
			"null gets a nil dereference in most languages")

	var resp ChangesResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &resp), "decode changes response")
	assert.Equal(0, resp.Count, "count")
	assert.False(resp.HasMore, "has_more")
	assert.Empty(resp.NextSince, "next_since echoes the absent request cursor")
	assert.Zero(resp.NextSinceID, "next_since_id echoes the absent request cursor")

	serverTime, err := time.Parse(time.RFC3339Nano, resp.ServerTime)
	require.NoErrorf(err, "server_time %q must parse as RFC3339", resp.ServerTime)
	assert.WithinDuration(time.Now().UTC(), serverTime, time.Hour,
		"server_time must be the database's clock reading, not a zero time")
}

// TestChangesEndpoint_EmptyPageEchoesRequestCursor covers a caught-up consumer.
// There is no last row to derive a cursor from, so the response echoes the
// request's cursor; zero values would replay the whole archive on the next poll
// of an idle feed.
func TestChangesEndpoint_EmptyPageEchoesRequestCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	first := getChangesPage(t, srv, changesTarget("", 0, 10))
	require.Len(first.Messages, 3, "first page")

	caughtUp := getChangesPage(t, srv, changesTarget(first.NextSince, first.NextSinceID, 10))
	require.Empty(caughtUp.Messages, "the consumer is caught up")
	assert.Equal(first.NextSince, caughtUp.NextSince,
		"an empty page echoes the requested since so the consumer holds its place")
	assert.Equal(first.NextSinceID, caughtUp.NextSinceID,
		"an empty page echoes the requested since_id so the consumer holds its place")
	assert.False(caughtUp.HasMore, "has_more")

	// The echoed cursor must itself be re-sendable, which is the whole point.
	stillCaughtUp := getChangesPage(t, srv, changesTarget(caughtUp.NextSince, caughtUp.NextSinceID, 10))
	assert.Empty(stillCaughtUp.Messages, "polling an idle feed must stay empty")
}

// TestChangesEndpoint_CursorAboveTheDatabaseClockRecovers is the other half of
// the echo: a cursor the feed can never satisfy must not be echoed back.
//
// The page query stops strictly below the database clock, so a cursor above
// that clock matches nothing — and the response is 200 / count=0 /
// has_more=false, byte-identical to "you are caught up". Echoing that cursor
// hands the poison straight back, so the consumer polls a stalled feed forever
// while the archive changes. A backwards clock step (NTP correction, a resumed
// VM, a restore onto slower hardware) or a client that builds its own cursor
// gets there without doing anything wrong.
func TestChangesEndpoint_CursorAboveTheDatabaseClockRecovers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	seedChangedMessages(t, st, 2)
	settleChangesClock(t, srv)

	future := changesServerTime(t, srv).Add(time.Hour).UTC().Format(changesTimeLayout)
	poisoned := getChangesPage(t, srv, changesTarget(future, 0, 100))
	require.Zero(poisoned.Count, "a cursor an hour ahead of the clock matches nothing")

	serverTime, err := time.Parse(time.RFC3339Nano, poisoned.ServerTime)
	require.NoError(err)
	nextSince, err := time.Parse(time.RFC3339Nano, poisoned.NextSince)
	require.NoError(err)
	assert.Falsef(nextSince.After(serverTime),
		"next_since %s is above server_time %s, so the very next poll is unsatisfiable too",
		poisoned.NextSince, poisoned.ServerTime)

	// Changes arrive after the poisoned poll and must be delivered.
	seedMoreChangedMessages(t, st, "late", 3)
	settleChangesClock(t, srv)

	resumed := getChangesPage(t, srv, changesTarget(poisoned.NextSince, poisoned.NextSinceID, 100))
	assert.NotZero(resumed.Count,
		"the feed stalled: changes made after the cursor was clamped were never delivered")
}

// TestChangesEndpoint_ClampsLimit pins the page-size rules a client can rely on
// and the boundary the store handoff makes dangerous: a non-positive limit
// reaching the store returns a zero server_time and no database round trip, so
// the default has to be applied first.
func TestChangesEndpoint_ClampsLimit(t *testing.T) {
	srv, st := newChangesServer(t)
	seedChangedMessages(t, st, maxPageSize+1)
	settleChangesClock(t, srv)

	tests := []struct {
		name   string
		target string
		want   int
	}{
		{"absent falls back to the default", "/api/v1/messages/changes", defaultChangesPageSize},
		{"above the maximum clamps", "/api/v1/messages/changes?limit=100000", maxPageSize},
		{"zero falls back to the default", "/api/v1/messages/changes?limit=0", defaultChangesPageSize},
		{"negative falls back to the default", "/api/v1/messages/changes?limit=-5", defaultChangesPageSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			resp := getChangesPage(t, srv, tc.target)
			assert.Len(resp.Messages, tc.want, "page size")
			assert.Equal(tc.want, resp.Count, "count")
			assert.True(resp.HasMore, "rows remain beyond a clamped page")
			serverTime, err := time.Parse(time.RFC3339Nano, resp.ServerTime)
			require.NoErrorf(err, "server_time %q must parse as RFC3339", resp.ServerTime)
			assert.False(serverTime.IsZero(),
				"server_time must come from the database clock: the store skips the "+
					"round trip entirely for a non-positive limit")
		})
	}
}

// changesLimitParam returns the published schema for the feed's limit query
// parameter, straight out of the document the OpenAPI artifacts and the
// generated clients are built from.
func changesLimitParam(t *testing.T) *huma.Param {
	t.Helper()
	path := OpenAPIDocument().Paths["/api/v1/messages/changes"]
	require.NotNil(t, path, "the changes endpoint must be in the OpenAPI document")
	require.NotNil(t, path.Get, "the changes endpoint must document its GET")
	for _, p := range path.Get.Parameters {
		if p.Name == "limit" {
			return p
		}
	}
	require.FailNow(t, "the changes endpoint must document a limit parameter")
	return nil
}

// TestChangesEndpoint_PublishedLimitRangeMatchesWhatTheServerAccepts stops the
// schema and the handler contradicting each other.
//
// The handler clamps limit rather than rejecting it: zero and negative values
// fall back to the default, oversized ones to the cap, and all of them answer
// 200. A published minimum/maximum turns those same requests into client-side
// validation failures — the generated Go client refuses to send a request the
// server would have answered — and contradicts the parameter's own description.
func TestChangesEndpoint_PublishedLimitRangeMatchesWhatTheServerAccepts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)
	seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), 1, 2, 3)

	limit := changesLimitParam(t)
	require.NotNil(limit.Schema, "the limit parameter must carry a schema")

	for _, raw := range []int64{0, -1, maxPageSize + 1, 1_000_000} {
		w := doGet(srv, fmt.Sprintf("/api/v1/messages/changes?limit=%d", raw))
		require.Equalf(http.StatusOK, w.Code,
			"the server clamps limit=%d rather than rejecting it: %s", raw, w.Body.String())

		if limit.Schema.Minimum != nil {
			assert.LessOrEqualf(*limit.Schema.Minimum, float64(raw),
				"the schema publishes minimum %v, so a generated client refuses to send "+
					"limit=%d — which the server answers with 200", *limit.Schema.Minimum, raw)
		}
		if limit.Schema.Maximum != nil {
			assert.GreaterOrEqualf(*limit.Schema.Maximum, float64(raw),
				"the schema publishes maximum %v, so a generated client refuses to send "+
					"limit=%d — which the server answers with 200", *limit.Schema.Maximum, raw)
		}
	}
}

// TestChangesEndpoint_ExactPageBoundaryReportsNoMorePages pins how has_more is
// computed. A page that exactly fills the limit is indistinguishable from a
// partial one unless the handler looks one row further, and a spurious
// has_more sends every caught-up consumer round again.
func TestChangesEndpoint_ExactPageBoundaryReportsNoMorePages(t *testing.T) {
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	resp := getChangesPage(t, srv, changesTarget("", 0, len(ids)))

	assert.Equal(ids, changedIDs(resp), "the page holds every row")
	assert.False(resp.HasMore,
		"a page that exactly fills the limit with nothing after it must report "+
			"has_more false")
}

// TestChangesEndpoint_RejectsMalformedCursor: a bad cursor is rejected rather
// than silently read as a zero value, which would replay the entire archive.
// The rejection happens before the store is consulted, so a client typo is
// reported as the typo it is whatever backend is configured.
func TestChangesEndpoint_RejectsMalformedCursor(t *testing.T) {
	srv, st := newChangesServer(t)
	seedChangedMessages(t, st, 1)

	tests := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"since not a timestamp", "/api/v1/messages/changes?since=yesterday", "invalid_since"},
		{"since_id not numeric", "/api/v1/messages/changes?since_id=abc", "invalid_since_id"},
		{"limit not numeric", "/api/v1/messages/changes?limit=many", "invalid_limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := doGet(srv, tc.target)
			require.Equalf(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			env := decodeErrorEnvelope(t, w)
			assert.Equal(t, tc.wantCode, env.Error, "error code")
		})
	}
}

// TestChangesEndpoint_EmptyCursorStartsFromTheBeginning pins what an EMPTY
// parameter value means, as opposed to an unparseable one. `?since=` is read as
// absent — the same as omitting it — across this whole API, so a client whose
// serialiser writes empty query parameters gets the first-run behaviour rather
// than a 400. It is the surprising half of the cursor contract, so the docs
// state it and this holds them to it.
func TestChangesEndpoint_EmptyCursorStartsFromTheBeginning(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	resp := getChangesPage(t, srv, "/api/v1/messages/changes?since=&since_id=5&limit=10")

	require.Equal(ids, changedIDs(resp),
		"an empty since is absent, not a parse failure, so the feed starts from "+
			"the beginning of the archive")
	assert.Equal(len(ids), resp.Count, "count")
	assert.False(resp.HasMore, "the whole archive fits in one page here")
}

// TestChangesEndpoint_ReportsDeletedMessages: removals are changes. A consumer
// that never learns a message was hidden or deleted at the source keeps
// mirroring something the archive no longer shows.
func TestChangesEndpoint_ReportsDeletedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	live, hidden, removed := ids[0], ids[1], ids[2]
	hiddenAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	removedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	setChangesMessageTimestamp(t, st, hidden, "deleted_at", hiddenAt)
	setChangesMessageTimestamp(t, st, removed, "deleted_from_source_at", removedAt)
	settleChangesClock(t, srv)

	resp := getChangesPage(t, srv, changesTarget("", 0, 10))
	byID := make(map[int64]ChangedMessageJSON, len(resp.Messages))
	for _, m := range resp.Messages {
		byID[m.ID] = m
	}
	require.Contains(byID, live, "a live message must appear in the feed")
	require.Contains(byID, hidden, "a dedup-hidden message must appear in the feed")
	require.Contains(byID, removed, "a source-deleted message must appear in the feed")

	assert.Nil(byID[live].DeletedAt, "a live message carries no deleted_at")
	assert.Nil(byID[live].DeletedFromSourceAt, "a live message carries no deleted_from_source_at")
	require.NotNil(byID[hidden].DeletedAt, "deleted_at must be reported, not just implied")
	require.NotNil(byID[removed].DeletedFromSourceAt, "deleted_from_source_at must be reported")

	gotHiddenAt, err := time.Parse(time.RFC3339Nano, *byID[hidden].DeletedAt)
	require.NoErrorf(err, "deleted_at %q must parse as RFC3339", *byID[hidden].DeletedAt)
	assert.WithinDuration(hiddenAt, gotHiddenAt, time.Second, "deleted_at value")
	gotRemovedAt, err := time.Parse(time.RFC3339Nano, *byID[removed].DeletedFromSourceAt)
	require.NoErrorf(err, "deleted_from_source_at %q must parse as RFC3339",
		*byID[removed].DeletedFromSourceAt)
	assert.WithinDuration(removedAt, gotRemovedAt, time.Second, "deleted_from_source_at value")
}

// TestChangesEndpoint_UnavailableWhenStoreLacksSupport covers the optional
// interface. A store that cannot answer the watermark query must produce a
// defined refusal, not a panic and not an empty 200 that a consumer would read
// as "nothing changed".
func TestChangesEndpoint_UnavailableWhenStoreLacksSupport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := newTestServerWithEngine(t, &querytest.MockEngine{})
	require.NotImplements((*ChangedMessageLister)(nil), srv.store,
		"this test only means something with a store that lacks the feed")

	w := doGet(srv, changesTarget("", 0, 0))

	require.Equalf(http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("feature_unavailable", env.Error, "error code")
}

// stubChangedMessageLister answers the feed with a fixed page, so a handler test
// can present a row no real store produces.
type stubChangedMessageLister struct {
	*mockStore

	page store.ChangedMessagePage
}

func (s *stubChangedMessageLister) ListChangedMessages(
	_ context.Context, _ time.Time, _ int64, _ int,
) (store.ChangedMessagePage, error) {
	return s.page, nil
}

// TestChangesEndpoint_UnreadableWatermarkDoesNotRewindTheCursor is the handler's
// half of the same defence the store makes: whatever a store reports for a row's
// watermark, the cursor the response publishes can never sit below the cursor
// the request carried. A zero watermark reaching next_since tells the consumer
// to resume from year 1, and it re-reads the whole archive on every poll from
// then on.
func TestChangesEndpoint_UnreadableWatermarkDoesNotRewindTheCursor(t *testing.T) {
	assert := assert.New(t)
	since := time.Date(2026, 3, 4, 5, 6, 7, 891011000, time.UTC)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				// ContentChangedAt left zero: the shape a row whose stored value
				// could not be parsed arrives in.
				Messages:   []store.ChangedMessage{{ID: 42}},
				ServerTime: since.Add(time.Second),
			},
		},
		Logger: testLogger(),
	})

	resp := getChangesPage(t, srv, changesTarget(since.Format(time.RFC3339Nano), 7, 10))

	assert.Equal(since.Format(changesTimeLayout), resp.NextSince,
		"next_since must be floored at the requested cursor, never rewound to the "+
			"zero time")
	assert.Equal(int64(42), resp.NextSinceID,
		"next_since_id still comes from the last row, so flooring next_since back "+
			"to the requested cursor does not also hand back the requested "+
			"since_id of 7")
}

// seedSparseChangedMessages inserts the two shapes whose JSON is mostly holes:
// an email that was never deleted and carries no platform timestamps, and a
// chat message with no subject, no snippet, and no platform id. Returns their
// ids in that order.
func seedSparseChangedMessages(t *testing.T, st *store.Store) (email, chat int64) {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "sparse@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(
		src.ID, "sparse-conv", "email_thread", "Sparse thread")
	require.NoError(t, err, "EnsureConversationWithType")

	email, err = st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		SourceMessageID: "sparse-email-1",
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: "Q4 planning", Valid: true},
		Snippet:         sql.NullString{String: "Here's the draft", Valid: true},
		SentAt:          sql.NullTime{Time: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC), Valid: true},
		SizeEstimate:    8412,
	})
	require.NoError(t, err, "UpsertMessage email")

	// No subject, no snippet, no platform id: the store COALESCEs all three to
	// the empty string, which is exactly what a `required` declaration rejects.
	chat, err = st.UpsertMessage(&store.Message{
		SourceID:       src.ID,
		ConversationID: convID,
		MessageType:    "imessage",
		SentAt:         sql.NullTime{Time: time.Date(2026, 3, 2, 11, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(t, err, "UpsertMessage chat")
	return email, chat
}

// TestChangesEndpoint_PagesSatisfyTheGeneratedClientContract feeds the handler's
// real output into the published Go client's own model and validator.
//
// The API and store tests decode into this package's structs, which accept
// anything; the contract a consumer actually holds is the generated one, where a
// field declared required is rejected when it is absent OR empty. That gap is
// how a feed whose every row omits something shipped a client that refused its
// own server's ordinary 200s.
func TestChangesEndpoint_PagesSatisfyTheGeneratedClientContract(t *testing.T) {
	t.Run("a page of live and sparse rows", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		srv, st := newChangesServer(t)
		email, chat := seedSparseChangedMessages(t, st)
		settleChangesClock(t, srv)

		page := decodeGeneratedChangesPage(t, srv, changesTarget("", 0, 10))
		require.NoError(page.Validate(),
			"the generated client must accept an ordinary page: a live message has "+
				"no deletion timestamps and a chat message has no subject")

		rows := make(map[int64]generated.ChangedMessageJSON, len(page.Messages))
		for _, row := range page.Messages {
			rows[row.ID] = row
		}
		require.Contains(rows, email, "the email row must be in the page")
		require.Contains(rows, chat, "the chat row must be in the page")

		// Without these the validation above could pass vacuously on rows that
		// happened to be fully populated.
		assert.Nil(rows[email].ReceivedAt, "the email has no received_at")
		assert.Nil(rows[email].DeletedAt, "the email was never deleted")
		assert.Nil(rows[email].DeletedFromSourceAt, "the email is still at the source")
		assert.Nil(rows[chat].Subject, "the chat message has no subject")
		assert.Nil(rows[chat].Snippet, "the chat message has no snippet")
		assert.Nil(rows[chat].SourceMessageID, "the chat message has no platform id")
	})

	t.Run("the first poll of an empty archive", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		srv, _ := newChangesServer(t)

		page := decodeGeneratedChangesPage(t, srv, changesTarget("", 0, 10))
		require.NoError(page.Validate(),
			"a caller that has never polled sends no cursor, so there is none to "+
				"echo back and next_since is absent")
		assert.Empty(page.Messages, "messages")
		assert.Nil(page.NextSince, "next_since")
		assert.NotEmpty(page.ServerTime, "server_time is always a clock reading")
	})
}

// decodeGeneratedChangesPage serves one feed request and decodes the response
// into the published client's model rather than this package's.
func decodeGeneratedChangesPage(t *testing.T, srv *Server, target string) generated.ChangesResponse {
	t.Helper()
	w := doGet(srv, target)
	require.Equalf(t, http.StatusOK, w.Code, "GET %s: %s", target, w.Body.String())
	var page generated.ChangesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page),
		"decode the response into the generated client model")
	return page
}

// TestChangesResponseFieldsAreAllTracked asserts the feed's correctness
// invariant, which is ONE-DIRECTIONAL: every JSON field of a response item must
// be tracked by the content_changed_at triggers. A field outside the tracked
// set would be cached stale by a consumer forever, because no trigger can
// invalidate it.
//
// The converse does NOT hold and is deliberately not asserted:
// MessagesContentColumns legitimately contains columns the feed does not return
// (sender_id and metadata are tracked because changing them means "re-read this
// message", but neither is in the response).
//
// Two exemptions:
//   - id, source_id: immutable identity, so no trigger is needed.
//   - content_changed_at: the watermark itself. It is classified as
//     non-content — a trigger keying off it would recurse — but it is
//     necessarily present in every response item as the cursor.
func TestChangesResponseFieldsAreAllTracked(t *testing.T) {
	assert := assert.New(t)
	exempt := []string{"id", "source_id", "content_changed_at"}

	for field := range reflect.TypeFor[ChangedMessageJSON]().Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if slices.Contains(exempt, name) {
			continue
		}
		assert.Containsf(store.MessagesContentColumns, name,
			"the feed reports %q, so a change to messages.%s must move "+
				"content_changed_at; otherwise every consumer caches it stale forever",
			name, name)
	}
}

// TestChangesEndpoint_PublishesHowFarItIsComplete covers the field a consumer
// needs to tell a quiet archive from a blocked feed.
//
// The page stops below the oldest write that could still commit, so an open
// write transaction pins it. In that state the response is otherwise
// indistinguishable from being caught up — no rows, has_more false — while
// server_time keeps moving. complete_through is what says which one it is.
func TestChangesEndpoint_PublishesHowFarItIsComplete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 2)
	settleChangesClock(t, srv)

	caughtUp := getChangesPage(t, srv, changesTarget("", 0, 10))
	require.Len(caughtUp.Messages, 2, "the seeded messages must be delivered first")
	completeThrough, err := time.Parse(time.RFC3339Nano, caughtUp.CompleteThrough)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", caughtUp.CompleteThrough)
	serverTime, err := time.Parse(time.RFC3339Nano, caughtUp.ServerTime)
	require.NoErrorf(err, "server_time %q must parse as RFC3339", caughtUp.ServerTime)
	assert.Falsef(completeThrough.After(serverTime),
		"complete_through %s is after server_time %s: the feed cannot be complete "+
			"through an instant the database clock has not reached",
		caughtUp.CompleteThrough, caughtUp.ServerTime)

	// A writer stamps a change and holds its transaction open.
	tx, err := st.DB().BeginTx(context.Background(), nil)
	require.NoError(err, "begin the pending write")
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "pending", ids[0])
	require.NoError(err, "stamp the pending change")

	held := getChangesPage(t, srv,
		changesTarget(caughtUp.NextSince, caughtUp.NextSinceID, 10))
	assert.Empty(held.Messages, "an uncommitted change must not be reported")
	heldThrough, err := time.Parse(time.RFC3339Nano, held.CompleteThrough)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", held.CompleteThrough)
	heldServerTime, err := time.Parse(time.RFC3339Nano, held.ServerTime)
	require.NoErrorf(err, "server_time %q must parse as RFC3339", held.ServerTime)
	assert.Truef(heldServerTime.After(heldThrough),
		"the clock reads %s and the feed claims to be complete through %s, with a "+
			"write still pending: a held-back feed that publishes complete_through "+
			"== server_time is telling a consumer it is caught up when it is not",
		held.ServerTime, held.CompleteThrough)

	require.NoError(tx.Commit(), "commit the pending change")
	deadline := time.Now().Add(20 * time.Second)
	var resumed ChangesResponse
	for {
		resumed = getChangesPage(t, srv,
			changesTarget(held.NextSince, held.NextSinceID, 10))
		if len(resumed.Messages) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.Len(resumed.Messages, 1, "the committed change must arrive")
	assert.Equal(ids[0], resumed.Messages[0].ID, "the changed message")
	resumedThrough, err := time.Parse(time.RFC3339Nano, resumed.CompleteThrough)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", resumed.CompleteThrough)
	assert.Truef(resumedThrough.After(heldThrough),
		"complete_through stayed at %s once the write finished: a bound that never "+
			"recovers is a stalled feed, not a cautious one", resumed.CompleteThrough)
}

// TestChangesEndpoint_StalledFeedIsLogged is the operator's half of the same
// signal. complete_through tells the consumer; nothing tells whoever runs the
// server, and the cause — a connection sitting inside a transaction — is theirs
// to fix, not the consumer's.
func TestChangesEndpoint_StalledFeedIsLogged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	logs := &bytes.Buffer{}
	stalledFor := 9 * time.Minute
	serverTime := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				ServerTime:      serverTime,
				CompleteThrough: serverTime.Add(-stalledFor),
			},
		},
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	resp := getChangesPage(t, srv, changesTarget("", 0, 10))
	require.Empty(resp.Messages, "the stub serves an empty page")

	assert.Contains(logs.String(), "message change feed is not advancing",
		"a feed that has stopped advancing must reach the operator's logs: the "+
			"response says so, but nobody running the server is reading someone "+
			"else's polling responses")
	assert.Contains(logs.String(), stalledFor.String(),
		"the log line must carry how far behind the feed is, so an operator can "+
			"tell a momentary batch from a connection left open since Tuesday")

	for range 5 {
		getChangesPage(t, srv, changesTarget("", 0, 10))
	}
	assert.Equal(1, strings.Count(logs.String(), "message change feed is not advancing"),
		"consumers poll, so the condition is re-observed on every request; one "+
			"stuck connection must not become a log flood")
}

// TestChangesEndpoint_FeedWithNoBoundYetLogsAFiniteLag covers the one state in
// which complete_through is not an instant the feed reached but the absence of
// one.
//
// A store that has never established a commit bound reports the zero time — on
// SQLite, a server that has not yet caught the database with its write lock
// free, which a restart during a bulk import produces. The page is correct (it
// is complete through nothing, so it carries no rows and moves no cursor), but
// subtracting year 1 from now saturates time.Duration, and the operator's
// warning then reads "lag=2562047h47m17s", which looks like a corrupt clock
// rather than a server that started a moment ago. The lag is not merely large
// here; it is undefined, and the log has to say which of the two it is.
func TestChangesEndpoint_FeedWithNoBoundYetLogsAFiniteLag(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	logs := &bytes.Buffer{}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				ServerTime: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
				// CompleteThrough left zero: no bound established yet.
			},
		},
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	resp := getChangesPage(t, srv, changesTarget("", 0, 10))
	require.Empty(resp.Messages, "a feed with no bound can publish no rows")

	assert.Contains(logs.String(), "message change feed is not advancing",
		"a feed that has never established a bound is not advancing, and the "+
			"operator has the same problem to fix as any other stall")
	assert.NotContains(logs.String(), "2562047h",
		"the lag against the zero time saturates time.Duration; reporting the "+
			"saturated value tells an operator their clock is broken when what "+
			"actually happened is that no bound has been established yet")
	assert.Contains(logs.String(), "no commit bound",
		"the cause must distinguish 'a transaction has been open for N minutes' "+
			"from 'nothing has ever been proved committed', because they are fixed "+
			"differently")
}

// TestChangesEndpoint_CompleteThroughIsAReachabilityBoundNotACursor pins what
// complete_through actually promises, which is weaker than it reads.
//
// It bounds what the feed is COMPLETE through, not what this response handed
// over: when the page filled, everything between the last row and that instant
// is still waiting behind next_since. The published wording used to say the
// change had "been offered to you", and a consumer that believed it and set its
// next cursor from complete_through skipped every one of those rows silently.
// So the property is two-sided — the gap is real (a consumer must not treat the
// bound as a cursor), and following next_since closes it completely.
func TestChangesEndpoint_CompleteThroughIsAReachabilityBoundNotACursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)
	seeded := seedChangedMessages(t, st, 12)
	settleChangesClock(t, srv)

	first := getChangesPage(t, srv, changesTarget("", 0, 3))
	require.True(first.HasMore, "a page of 3 out of 12 must report more to come")
	bound, err := time.Parse(time.RFC3339Nano, first.CompleteThrough)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", first.CompleteThrough)

	below := countMessagesStampedBelow(t, st, bound)
	assert.Greaterf(below, first.Count,
		"complete_through %s stands above %d committed changes but the page carried "+
			"%d: a consumer that resumed from the bound would skip the difference, "+
			"which is why it must never be used as a cursor",
		first.CompleteThrough, below, first.Count)

	// Following next_since instead is what the guarantee is actually about.
	delivered := map[int64]bool{}
	page := first
	for range 20 {
		for _, m := range page.Messages {
			delivered[m.ID] = true
		}
		if !page.HasMore {
			break
		}
		page = getChangesPage(t, srv, changesTarget(page.NextSince, page.NextSinceID, 3))
	}
	assert.False(page.HasMore, "the walk must reach the end of the feed")
	for _, id := range seeded {
		assert.Truef(delivered[id],
			"message %d was committed below the first page's complete_through (%s) and "+
				"following next_since never produced it: the bound would then promise "+
				"something the cursor does not deliver", id, first.CompleteThrough)
	}
}

// countMessagesStampedBelow counts the rows whose watermark is strictly below
// instant, binding it the way each backend compares watermarks: PostgreSQL
// parses a real timestamptz, SQLite compares the trigger's textual format
// lexically.
func countMessagesStampedBelow(t *testing.T, st *store.Store, instant time.Time) int {
	t.Helper()
	var arg any = instant.UTC()
	if !st.IsPostgreSQL() {
		arg = instant.UTC().Format(store.SQLiteTimestampLayout)
	}
	var n int
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT count(*) FROM messages WHERE content_changed_at < ?`), arg).Scan(&n),
		"count the committed changes below the bound")
	return n
}

// TestChangesEndpoint_FutureCursorClampsToTheCommitBoundNotTheClock pins that
// recovering a consumer whose cursor is above the database clock never moves that
// cursor above a change that is stamped but not yet committed.
//
// While a writer holds an open transaction, complete_through sits strictly below
// server_time and the in-flight row's watermark sits between them. Clamping to
// server_time -- which this did -- places the cursor above that row, and when the
// writer commits the row is below the cursor forever. The bound is by
// construction below every write it can see, so a cursor placed there cannot
// skip one. The writes it cannot see, and every other exception to what the
// feed delivers, are enumerated in one place: docs/api-server.md's delivery
// contract.
func TestChangesEndpoint_FutureCursorClampsToTheCommitBoundNotTheClock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	serverTime := time.Date(2026, 7, 26, 10, 0, 30, 0, time.UTC)
	// A writer has been open since :10, so the feed is complete only through :10
	// even though the clock reads :30. That writer's row, stamped at :20, is
	// still uncommitted.
	completeThrough := time.Date(2026, 7, 26, 10, 0, 10, 0, time.UTC)
	inFlightStamp := time.Date(2026, 7, 26, 10, 0, 20, 0, time.UTC)

	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				Messages:        nil,
				ServerTime:      serverTime,
				CompleteThrough: completeThrough,
			},
		},
		Logger: testLogger(),
	})

	future := serverTime.Add(time.Hour)
	resp := getChangesPage(t, srv, changesTarget(future.Format(changesTimeLayout), 7, 10))

	got, err := time.Parse(changesTimeLayout, resp.NextSince)
	require.NoError(err, "next_since must parse")

	// Equality, not "not after". A merely-lower cursor is satisfied by the zero
	// time, and an implementation that rewound the consumer to the start of the
	// archive on every future cursor -- which this plan explicitly rejects --
	// would pass a `not after` assertion while being badly wrong.
	assert.Truef(got.Equal(completeThrough),
		"the recovered cursor must be exactly the commit bound (%s), got %s; "+
			"anything above it skips the change stamped at %s by the still-open "+
			"writer, and anything below it replays the archive",
		completeThrough, got, inFlightStamp)
	assert.Equal(int64(0), resp.NextSinceID,
		"the id tiebreak belonged to a different instant and must be reset")
}

// TestChangesEndpoint_FutureCursorIsEchoedWhenNoBoundIsEstablished pins the one
// case where the clamp must not fire. A server that has never taken a bound
// reading reports complete_through as the zero time; clamping down to it would
// replay the whole archive, and clamping to the clock would be the unsafe move
// this fix removes. Echoing holds the consumer's place until the bound resolves.
func TestChangesEndpoint_FutureCursorIsEchoedWhenNoBoundIsEstablished(t *testing.T) {
	assert := assert.New(t)

	serverTime := time.Date(2026, 7, 26, 10, 0, 30, 0, time.UTC)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				Messages:        nil,
				ServerTime:      serverTime,
				CompleteThrough: time.Time{}, // no bound established yet
			},
		},
		Logger: testLogger(),
	})

	future := serverTime.Add(time.Hour)
	sent := future.Format(changesTimeLayout)
	// A nonzero tiebreak, so the echo is checked as a whole composite cursor.
	// Resetting the id here would re-deliver the start of that instant on every
	// poll, which the clamp branch accepts deliberately but this branch must not:
	// nothing has been clamped, so there is nothing to re-deliver.
	resp := getChangesPage(t, srv, changesTarget(sent, 7, 10))

	assert.Equal(sent, resp.NextSince,
		"with no bound established the cursor must be echoed unchanged, not "+
			"clamped to the clock and not reset to the zero time")
	assert.Equal(int64(7), resp.NextSinceID,
		"and its tiebreak must be echoed with it -- this branch clamps nothing, "+
			"so resetting the id would re-deliver that instant on every poll")
}
