package slack

// sweep_parking_test.go — skipped regression tests pinning the sweep's
// convergence contract ("repeated limited runs converge", sweep.go:44) for
// the three probe-verified parked-floor regimes of the --limit 1 sweep
// (docs/plans/2026-08-07-slack-sweep-utc-flake.md §2; probe blueprints in
// the plan's reports/V3.md):
//
//	E1  floor lands just after a UTC midnight (00:03Z), inside
//	    [midnight, midnight+sweepLagMargin)
//	E2  floor lands exactly ON a UTC midnight
//	E3  an ordinary day-behind --limit 1 backlog naturally commits its
//	    floor to exactly midnight(D+1) on run 1, then parks
//
// Each test asserts the CORRECT post-fix behavior — a late reply is archived
// by repeated --limit 1 runs — and is skipped while the production bug
// stands. Removing the skip is the one-line act that turns each into a live
// regression test once the fix lands. The bodies stay well under the 7-day
// canonicalThreadAuditInterval so the audit backstop can never mask a parked
// sweep.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testClockMidnight returns the UTC midnight of testClockBase's day — the
// day boundary every parked-floor regime here is positioned against.
func testClockMidnight() time.Time {
	y, m, d := testClockBase.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// pinImporterClock re-anchors imp.now at `at`, advancing with real elapsed
// time from the moment of the call — the pinned-clock pattern the factored
// sweep-convergence tests established, re-pinned once per probe step so a
// multi-instant run schedule stays drift-free.
func pinImporterClock(imp *Importer, at time.Time) {
	anchor := time.Now()
	imp.now = func() time.Time { return at.Add(time.Since(anchor)) }
}

// addLateReply appends a new reply to C09's ancient root at replyTS (a
// message created at the source AFTER the initial backfill), returning its
// source_message_id.
func addLateReply(f *fakeSlack, rootTS, replyTS string) string {
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: replyTS, ThreadTS: rootTS, User: "UME", Text: "late reply"})
	f.mu.Unlock()
	return "C09:" + replyTS
}

// requireReplyArchived asserts the sweep's convergence contract: the late
// reply reached the archive through the repeated --limit 1 runs.
func requireReplyArchived(t *testing.T, imp *Importer, sourceMessageID string) {
	t.Helper()
	var n int
	require.NoError(t, imp.store.DB().QueryRow(imp.store.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), sourceMessageID).Scan(&n))
	require.Equal(t, 1, n,
		"repeated --limit 1 runs must converge (sweep.go:44); archived=0 means the sweep parked at its floor")
}

// TestSweepConvergesFloorJustAfterMidnight_KnownBug pins the E1 regime: the
// initial backfill's floor lands at 00:03Z — inside the post-midnight
// lag-margin band — so overlapFloor(floor) reaches back to the PREVIOUS day
// and every --limit 1 run burns its whole budget re-searching that
// already-certified day. A late reply on the floor's own day must still be
// archived by repeated --limit 1 runs.
func TestSweepConvergesFloorJustAfterMidnight_KnownBug(t *testing.T) {
	t.Skip("known production bug: --limit 1 sweep parks when floor lands within sweepLagMargin after midnight — see docs/plans/2026-08-07-slack-sweep-utc-flake.md §2; un-skip with the production fix")

	require := require.New(t)
	base := testClockMidnight().Add(3 * time.Minute) // 00:03:00Z on the clock's day
	f, rootTS := oldThreadWorkspaceAt(t, base)
	imp, opts := testImporter(t, f)

	// Initial unlimited import: the sweep stamps its floor at imp.now()
	// ≈ 00:03Z, inside [midnight, midnight+sweepLagMargin).
	pinImporterClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// A NEW reply lands on the floor's OWN day (00:03:02Z), above the pin.
	replyID := addLateReply(f, rootTS, tsFreshAt(base, 0))

	// Convergence: repeated --limit 1 runs on the V3 blueprint's schedule
	// must archive the reply.
	limited := opts
	limited.Limit = 1
	for _, offset := range []time.Duration{time.Minute, 30 * time.Minute, 2 * time.Hour} {
		pinImporterClock(imp, base.Add(offset))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireReplyArchived(t, imp, replyID)
}

// TestSweepConvergesFloorExactlyOnMidnight_KnownBug pins the E2 regime: a
// backfill just before midnight plus one --limit 1 run commits the floor to
// EXACTLY midnight(D+1) (every day-boundary commit is
// tsFormat(nextDayStart(...))). From that floor, nextBoundary == floor on
// the walk's first day, so no --limit 1 run can ever commit again. A reply
// on day D+1 must still be archived by repeated --limit 1 runs.
func TestSweepConvergesFloorExactlyOnMidnight_KnownBug(t *testing.T) {
	t.Skip("known production bug: --limit 1 sweep parks when floor lands within sweepLagMargin after midnight — see docs/plans/2026-08-07-slack-sweep-utc-flake.md §2; un-skip with the production fix")

	require := require.New(t)
	midnight := testClockMidnight()    // midnight(D+1)
	base := midnight.Add(-time.Second) // 23:59:59Z on day D
	f, rootTS := oldThreadWorkspaceAt(t, base)
	imp, opts := testImporter(t, f)

	// Initial unlimited import just before midnight: floor ≈ 23:59:59Z, day D.
	pinImporterClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// A NEW reply lands at 00:00:01Z — on day D+1, past the floor.
	replyID := addLateReply(f, rootTS, tsFreshAt(base, 0))

	// One --limit 1 run at +1m searches day D and commits the floor to
	// exactly midnight(D+1); it cannot see the D+1 reply. This run
	// manufactures the parked state.
	limited := opts
	limited.Limit = 1
	pinImporterClock(imp, base.Add(time.Minute))
	_, err = imp.Import(context.Background(), limited)
	require.NoError(err)

	// Convergence: further --limit 1 runs must reach day D+1 and archive
	// the reply.
	for _, offset := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		pinImporterClock(imp, midnight.Add(offset))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireReplyArchived(t, imp, replyID)
}

// TestSweepLimitOneConvergesFromDayBehindBacklog_KnownBug pins the E3
// regime: no near-midnight wall clock anywhere — an ordinary midday floor
// falls a day behind, and run 1's catch-up commit lands on exactly
// midnight(D+1) as a natural product of day-boundary commits. Subsequent
// --limit 1 runs must still traverse day D+1 and archive the reply, not
// park re-charging the overlap day.
func TestSweepLimitOneConvergesFromDayBehindBacklog_KnownBug(t *testing.T) {
	t.Skip("known production bug: --limit 1 sweep parks when floor lands within sweepLagMargin after midnight — see docs/plans/2026-08-07-slack-sweep-utc-flake.md §2; un-skip with the production fix")

	require := require.New(t)
	base := testClockMidnight().Add(-36 * time.Hour) // 12:00Z on day D, two days back
	f, rootTS := oldThreadWorkspaceAt(t, base)
	imp, opts := testImporter(t, f)

	// Initial unlimited import at midday of day D: an ordinary mid-day floor.
	pinImporterClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// A NEW reply lands on day D+1 at ~13:00Z — one full day behind by the
	// time the --limit 1 schedule resumes on day D+2.
	replyID := addLateReply(f, rootTS, tsFreshAt(base, 25*60*60))

	// Convergence: --limit 1 runs on day D+2. Run 1 commits exactly
	// midnight(D+1); later runs must keep advancing through day D+1 and
	// archive the reply.
	limited := opts
	limited.Limit = 1
	runNow := base.Add(48 * time.Hour) // 12:00Z on day D+2
	for i := range 5 {
		pinImporterClock(imp, runNow.Add(time.Duration(i)*time.Minute))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireReplyArchived(t, imp, replyID)
}
