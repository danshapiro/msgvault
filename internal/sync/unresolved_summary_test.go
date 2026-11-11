package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestSyncSummaryCountsCurrentMaterializationDebt(t *testing.T) {
	for _, mode := range []string{"full", "idle-incremental"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			blocked := filepath.Join(e.TmpDir, "blocked")
			require.NoError(os.WriteFile(blocked, []byte("not a directory"), 0600))
			opts := &Options{AttachmentsDir: blocked}
			e.Mock.AddMessage("pending-file", testMIMEWithAttachment(), []string{"INBOX"})
			summary, err := New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
			require.NoError(err)
			assert.Equal(int64(1), summary.UnresolvedMessages)
			source := e.CreateSource(t)
			for range 2 {
				service := New(e.Mock, e.Store, opts)
				if mode == "full" {
					summary, err = service.Full(t.Context(), testEmail)
				} else {
					summary, err = service.Incremental(t.Context(), source)
				}
				require.NoError(err)
				assert.Equal(int64(1), summary.UnresolvedMessages, "repeated attempts concern one message")
				source, err = e.Store.GetSourceByID(source.ID)
				require.NoError(err)
			}
			require.NoError(os.Remove(blocked))
			summary, err = New(e.Mock, e.Store, opts).Incremental(t.Context(), source)
			require.NoError(err)
			assert.Zero(summary.UnresolvedMessages, "successful local replay settles current debt")
			source, err = e.Store.GetSourceByID(source.ID)
			require.NoError(err)
			summary, err = New(e.Mock, e.Store, opts).Incremental(t.Context(), source)
			require.NoError(err)
			assert.Zero(summary.UnresolvedMessages, "an idle completed account remains healthy")
		})
	}
}

func TestSyncSummaryResumedHistoricalErrorsAreNotCurrentDebt(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := newTestEnv(t)
	source := e.CreateSource(t)
	runID, err := e.Store.StartSync(source.ID, "full")
	require.NoError(err)
	_, err = e.Store.DB().ExecContext(t.Context(), `UPDATE sync_runs SET cursor_after = '1000', request_fingerprint = ? WHERE id = ?`, e.Syncer.fullSyncRequestFingerprint(), runID)
	require.NoError(err)
	require.NoError(e.Store.UpdateSyncCheckpoint(runID, &store.Checkpoint{PageToken: "page_1", ErrorsCount: 18}))
	require.NoError(e.Store.FailSync(runID, "synthetic interrupted enumeration"))
	summary := runFullSync(t, e)
	assert.True(summary.WasResumed)
	assert.Equal(int64(18), summary.Errors, "preserve cumulative attempts for operator history")
	assert.Zero(summary.UnresolvedMessages, "historical attempts do not mean current missing messages")
}
