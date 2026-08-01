package store

// ParseDBTime is exported for testing unexported timestamp parsing behavior.
var ParseDBTime = parseDBTime

// SetFTS5AvailableForTest flips the cached availability flag. Tests use this
// to exercise the guarantee that RebuildFTS works even when FTS5 looks
// unavailable — the symptom that motivates a rebuild in the first place.
func SetFTS5AvailableForTest(s *Store, v bool) {
	s.fts5Available = v
}

// SetBackfillFTSBatchErrHookForTest installs (or, with nil, clears) the
// test-only hook that forces backfillFTSBatch to fail for a chosen id range.
// Tests use it to deterministically trigger backfillFTSRowByRow's
// skip-the-bad-row-and-continue fallback. Returns a restore func that clears
// the hook, so callers can defer it.
func SetBackfillFTSBatchErrHookForTest(fn func(fromID, toID int64) error) func() {
	backfillFTSBatchErrHook = fn
	return func() { backfillFTSBatchErrHook = nil }
}

// SetInitSchemaWindowHookForTest installs (or, with nil, clears) the test-only
// hook that fires inside InitSchema after the content_changed_at backfill has
// recorded itself and while the remaining index builds are still pending. Tests
// use it to perform a real INSERT exactly where a concurrent writer used to lose
// its watermark. Returns a restore func, so callers can defer it.
func SetInitSchemaWindowHookForTest(fn func()) func() {
	initSchemaWindowHook = fn
	return func() { initSchemaWindowHook = nil }
}
