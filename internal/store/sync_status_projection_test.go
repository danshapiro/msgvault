package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncStatusErrorProjectionPreservesLedgerAndSourceScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("gmail", "alice@example.test")
	require.NoError(err)
	other, err := st.GetOrCreateSource("gmail", "bob@example.test")
	require.NoError(err)
	old, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	add := func(run int64, key, phase, status, kind, message string) {
		require.NoError(st.RecordSyncRunItemContext(ctx, store.SyncRunItem{SyncRunID: run, SourceMessageID: key, Phase: phase, Status: status, ErrorKind: kind, ErrorMessage: message}))
	}
	add(old, "old-resolution", "ingest", store.SyncRunItemStatusResolved, "", "")
	require.NoError(st.CompleteSync(old, ""))
	run, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	for _, key := range []string{"unresolved", "other-source", "old-resolution", "wrong-phase", "settled"} {
		add(run, key, "ingest", store.SyncRunItemStatusError, "attachment_error", store.SyncRunItemFilesPendingMessage)
	}
	add(run, "genuine", "ingest", store.SyncRunItemStatusError, "attachment_error", "actual attachment failure")
	add(run, "wrong-kind", "ingest", store.SyncRunItemStatusError, "ingest_error", store.SyncRunItemFilesPendingMessage)
	require.NoError(st.CompleteSync(run, ""))
	later, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	foreign, err := st.StartSync(other.ID, "full")
	require.NoError(err)
	add(foreign, "other-source", "ingest", store.SyncRunItemStatusResolved, "", "")
	add(later, "wrong-phase", "download", store.SyncRunItemStatusResolved, "", "")
	for _, key := range []string{"settled", "genuine", "wrong-kind"} {
		add(later, key, "fetch", store.SyncRunItemStatusResolved, "", "")
	}
	rows, err := st.ListSyncRunItemsContext(ctx, run, store.SyncRunItemStatusError, 50)
	require.NoError(err)
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.SourceMessageID)
	}
	assert.ElementsMatch([]string{"unresolved", "other-source", "old-resolution", "wrong-phase", "genuine", "wrong-kind"}, keys)
	all, err := st.ListSyncRunItemsContext(ctx, run, "", 50)
	require.NoError(err)
	assert.Len(all, 7, "unfiltered durable history remains intact")
	count, err := st.CountSyncRunItemsContext(ctx, run, store.SyncRunItemStatusError)
	require.NoError(err)
	assert.Equal(int64(7), count)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = st.ListSyncRunItemsContext(canceled, run, store.SyncRunItemStatusError, 50)
	assert.ErrorIs(err, context.Canceled)
}

// A real ledger with many pending files and later unrelated outcomes exercises
// the status projection that used to rescan the later ledger for every item.
// Runtime is recorded for comparison, not asserted as a machine-speed contract.
func TestSyncStatusErrorProjectionLargeHistory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "large@example.test")
	require.NoError(err)
	run, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	require.NoError(st.CompleteSync(run, ""))
	later, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	ctx := t.Context()
	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	stmt, err := tx.PrepareContext(ctx, st.Rebind(`INSERT INTO sync_run_items(sync_run_id, source_message_id, phase, status, error_kind, error_message, created_at) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`))
	require.NoError(err)
	defer func() { _ = stmt.Close() }()
	// Every pending error must be settled before LIMIT, including the most recent.
	for i := range 1000 {
		_, err = stmt.ExecContext(ctx, run, fmt.Sprintf("pending-%d", i), "ingest", store.SyncRunItemStatusError, "attachment_error", store.SyncRunItemFilesPendingMessage)
		require.NoError(err)
	}
	for i := range 25000 {
		_, err = stmt.ExecContext(ctx, later, fmt.Sprintf("unrelated-%d", i), "ingest", store.SyncRunItemStatusResolved, "", "")
		require.NoError(err)
	}
	for i := range 1000 {
		_, err = stmt.ExecContext(ctx, later, fmt.Sprintf("pending-%d", i), "ingest", store.SyncRunItemStatusResolved, "", "")
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	started := time.Now()
	rows, err := st.ListSyncRunItemsContext(ctx, run, store.SyncRunItemStatusError, 20)
	require.NoError(err)
	assert.Empty(rows, "resolved rows are excluded before applying the page limit")
	t.Logf("1000 pending errors, 26000 later resolutions: %s", time.Since(started))
}

func TestSyncStatusErrorProjectionFiltersBeforeLimit(t *testing.T) {
	for _, kind := range []string{"attachment_error", "remote_image_error", "attachment_remote_image_error"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", "limit@example.test")
			require.NoError(err)
			run, err := st.StartSync(source.ID, "full")
			require.NoError(err)
			add := func(key, status, message string) {
				require.NoError(st.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{SyncRunID: run, SourceMessageID: key, Phase: "ingest", Status: status, ErrorKind: kind, ErrorMessage: message}))
			}
			for i := range 5 {
				add(fmt.Sprintf("genuine-%d", i), store.SyncRunItemStatusError, "download failed")
			}
			for i := range 30 {
				add(fmt.Sprintf("settled-%d", i), store.SyncRunItemStatusError, store.SyncRunItemFilesPendingMessage)
			}
			for i := range 30 {
				add(fmt.Sprintf("settled-%d", i), store.SyncRunItemStatusResolved, "")
			}
			rows, err := st.ListSyncRunItemsContext(t.Context(), run, store.SyncRunItemStatusError, 3)
			require.NoError(err)
			require.Len(rows, 3)
			assert.Equal("genuine-4", rows[0].SourceMessageID)
			assert.Equal("genuine-3", rows[1].SourceMessageID)
			assert.Equal("genuine-2", rows[2].SourceMessageID)
			resolved, err := st.ListSyncRunItemsContext(t.Context(), run, store.SyncRunItemStatusResolved, 50)
			require.NoError(err)
			assert.Len(resolved, 30)
		})
	}
}
