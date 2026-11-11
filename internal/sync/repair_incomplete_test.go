package sync

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestRepairMessageRejectsIncompleteCandidateBeforePublication(t *testing.T) {
	t.Parallel()
	assert, require := assert.New(t), require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	id := seedRepairRow(t, env.Store, source.ID, "gmail-incomplete", "old-thread", "old subject")
	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = attachmentsDir })
	oldAttachment := []byte("previous complete attachment")
	oldRaw := repairRaw(
		"gmail-incomplete", "old-thread", "old subject", "<p>previous complete body</p>", oldAttachment,
	)
	oldRaw.Raw = []byte(strings.Replace(string(oldRaw.Raw),
		`Content-Type: text/plain; charset="utf-8"`, `Content-Type: text/html; charset="utf-8"`, 1))
	env.Mock.Messages["gmail-incomplete"] = oldRaw
	_, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "gmail-incomplete", SourceID: source.ID})
	require.NoError(err)
	before := readRepairSnapshot(t, env.Store, id)
	beforeMessage, err := env.Store.GetMessageContext(t.Context(), id)
	require.NoError(err)
	require.NotEmpty(beforeMessage.BodyText)
	require.NotEmpty(beforeMessage.BodyHTML)
	beforeAttachments, err := env.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(beforeAttachments, 1)
	oldBlobPath := filepath.Join(attachmentsDir, beforeAttachments[0].StoragePath)
	oldBlob, err := os.ReadFile(oldBlobPath)
	require.NoError(err)
	require.Equal(oldAttachment, oldBlob)

	runID, err := env.Store.StartSync(source.ID, "full")
	require.NoError(err)
	require.NoError(env.Store.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID: runID, SourceMessageID: "gmail-incomplete", Phase: syncItemPhaseIngest,
		Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindAttachmentError,
		ErrorMessage: "earlier attachment recovery is unresolved",
	}))
	debtBefore, err := env.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debtBefore, 1)

	freshAttachment := []byte("candidate new valid attachment")
	brokenAttachment := []byte("candidate broken attachment")
	candidate := testemail.NewMessage().
		Subject("incomplete replacement").
		Body("candidate body must not replace the complete archive").
		WithAttachment("fresh.bin", "application/octet-stream", freshAttachment).
		WithAttachment("broken.bin", "application/octet-stream", brokenAttachment).Bytes()
	candidate = []byte(strings.Replace(string(candidate), base64.StdEncoding.EncodeToString(brokenAttachment), "S", 1))
	parsed, err := mime.Parse(candidate)
	require.NoError(err, "the regression is a partial parse with no returned error")
	require.True(parsed.Incomplete)
	var freshHash string
	for _, attachment := range parsed.Attachments {
		if bytes.Equal(attachment.Content, freshAttachment) {
			freshHash = attachment.ContentHash
		}
	}
	require.NotEmpty(freshHash, "candidate must have publishable content as well as its malformed part")
	freshBlobPath := filepath.Join(attachmentsDir, freshHash[:2], freshHash)
	require.NoFileExists(freshBlobPath)
	env.Mock.Messages["gmail-incomplete"] = &gmail.RawMessage{
		ID: "gmail-incomplete", ThreadID: "new-thread", Raw: candidate,
	}

	result, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "gmail-incomplete", SourceID: source.ID})
	require.ErrorContains(err, "incomplete")
	assert.Nil(result)
	assert.Equal(before, readRepairSnapshot(t, env.Store, id))
	afterMessage, err := env.Store.GetMessageContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(beforeMessage, afterMessage)
	afterBlob, err := os.ReadFile(oldBlobPath)
	require.NoError(err)
	assert.Equal(oldBlob, afterBlob)
	assert.NoFileExists(freshBlobPath, "strict preparation must reject before publishing any candidate blob")
	debtAfter, err := env.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Equal(debtBefore, debtAfter, "an inconclusive repair cannot settle the previous obligation")
}
