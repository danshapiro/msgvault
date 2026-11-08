package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPSTMIMERepairGuardsRawAndExistingAttachment(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-source-123")
	old := []byte("malformed raw MIME")
	corrected := []byte("corrected raw MIME")
	require.NoError(t, f.Store.UpsertMessageRaw(id, old))
	originalHash := strings.Repeat("a", 64)
	otherHash := strings.Repeat("b", 64)
	original := store.AttachmentWrite{
		Filename: "kept.txt", MIMEType: "text/plain", StoragePath: "aa/" + originalHash,
		ContentHash: originalHash, Size: 1, SourcePartKey: "mime:1",
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
	}
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), id, original))
	conflict := original
	conflict.ContentHash = otherHash
	conflict.StoragePath = "bb/" + otherHash
	_, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-source-123", []byte("stale"), corrected, nil, nil)
	require.ErrorContains(t, err, "changed")
	_, err = f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-source-123", old, corrected, []store.AttachmentWrite{conflict}, nil)
	require.ErrorContains(t, err, "conflicts")
	raw, err := f.Store.GetMessageRaw(id)
	require.NoError(t, err)
	assert.Equal(t, old, raw)
	refs, err := f.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, originalHash, refs[0].ContentHash)
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-source-123", old, corrected, []store.AttachmentWrite{original}, nil)
	require.NoError(t, err)
	assert.Zero(t, result.AttachmentsAdded)
	raw, err = f.Store.GetMessageRaw(id)
	require.NoError(t, err)
	assert.Equal(t, corrected, raw)
}

func TestPSTMIMERepairUpdatesOnlyUnchangedBodyAndSearch(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-body-123")
	old := []byte("old MIME")
	require.NoError(t, f.Store.UpsertMessageRaw(id, old))
	require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: "oldbodyword", Valid: true}, sql.NullString{}))
	require.NoError(t, f.Store.UpsertFTS(id, "", "oldbodyword", "", "", ""))
	_, err := f.Store.DB().Exec(`UPDATE messages SET snippet = ? WHERE id = ?`, "oldbodyword", id)
	require.NoError(t, err)
	body := &store.PSTRepairBody{ExpectedText: "oldbodyword", NewText: "recoveredbodyword"}
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-body-123", old, []byte("new MIME"), nil, body)
	require.NoError(t, err)
	assert.True(t, result.BodyUpdated)
	text, err := f.Store.GetMessageBodyText(id)
	require.NoError(t, err)
	assert.Equal(t, "recoveredbodyword", text)
	var snippet string
	require.NoError(t, f.Store.DB().QueryRow(`SELECT snippet FROM messages WHERE id = ?`, id).Scan(&snippet))
	assert.Equal(t, "recoveredbodyword", snippet)
	results, total, err := f.Store.SearchMessages("recoveredbodyword", 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, results, 1)
	assert.Equal(t, id, results[0].ID)

	require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: "existing custom text", Valid: true}, sql.NullString{}))
	result, err = f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-body-123", []byte("new MIME"), []byte("new MIME"), nil, body)
	require.NoError(t, err)
	assert.True(t, result.BodyPreserved)
	text, err = f.Store.GetMessageBodyText(id)
	require.NoError(t, err)
	assert.Equal(t, "existing custom text", text)
}

func TestPSTMIMERepairFillsOnlyEmptyMatchingAttachmentLocation(t *testing.T) {
	for _, key := range []string{"", "mime:1"} {
		t.Run("key="+key, func(t *testing.T) {
			f := storetest.New(t)
			id := f.CreateMessage("pst-attachment-123")
			require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
			hash := strings.Repeat("c", 64)
			original := store.AttachmentWrite{Filename: "keep-name", MIMEType: "application/octet-stream", ContentHash: hash, Size: 2, SourcePartKey: key}
			if key != "" {
				original.ContentHash = strings.ToUpper(hash)
			}
			require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), id, original))
			repair := original
			repair.SourcePartKey, repair.StoragePath = "mime:1", "cc/"+hash
			repair.ContentHash = hash
			if key != "" {
				repair.Filename = "source-name"
			}
			result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-attachment-123", []byte("raw"), []byte("raw"), []store.AttachmentWrite{repair}, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, result.AttachmentsAdded)
			refs, err := f.Store.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, refs, 1)
			assert.Equal(t, "keep-name", refs[0].Filename)
			assert.Equal(t, key, refs[0].SourcePartKey)
			assert.Equal(t, repair.StoragePath, refs[0].StoragePath)
		})
	}
}

func TestPSTMIMERepairKeepsDistinctLegacyAttachments(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-distinct-123")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	hash := strings.Repeat("d", 64)
	legacy := store.AttachmentWrite{Filename: "original-name", MIMEType: "application/octet-stream", ContentHash: hash, StoragePath: "dd/" + hash, Size: 2}
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), id, legacy))
	repair := legacy
	repair.Filename, repair.SourcePartKey = "different-name", "mime:1"
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-distinct-123", []byte("raw"), []byte("raw"), []store.AttachmentWrite{repair}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.AttachmentsAdded)
	refs, err := f.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, "original-name", refs[0].Filename)
	assert.Equal(t, "different-name", refs[1].Filename)
}

func TestPSTMIMERepairReportsExistingSkippedAttachment(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-skipped-123")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	hash := strings.Repeat("e", 64)
	write := store.AttachmentWrite{Filename: "source.bin", ContentHash: hash, StoragePath: "ee/" + hash, Size: 2, SourcePartKey: "mime:1"}
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), id, write))
	_, err := f.Store.DB().Exec(`UPDATE attachments SET attachment_state = 'skipped', attachment_skip_reason = 'policy' WHERE message_id = ?`, id)
	require.NoError(t, err)
	_, err = f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-skipped-123", []byte("raw"), []byte("new"), []store.AttachmentWrite{write}, nil)
	require.ErrorContains(t, err, "skipped")
	raw, err := f.Store.GetMessageRaw(id)
	require.NoError(t, err)
	assert.Equal(t, []byte("raw"), raw)
}

func TestPSTMIMERepairDoesNotClaimPreservedMergeAttachment(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("pst-merge-123")
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("raw")))
	hash := strings.Repeat("f", 64)
	write := store.AttachmentWrite{Filename: "same.bin", ContentHash: hash, StoragePath: "ff/" + hash, Size: 2}
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), id, write))
	_, err := f.Store.DB().Exec(`INSERT INTO source_merge_preserved_attachments (attachment_id) SELECT id FROM attachments WHERE message_id = ?`, id)
	require.NoError(t, err)
	write.SourcePartKey = "mime:1"
	result, err := f.Store.RepairPSTRawAndAttachments(t.Context(), f.Source.ID, id, "pst-merge-123", []byte("raw"), []byte("raw"), []store.AttachmentWrite{write}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.AttachmentsAdded)
	refs, err := f.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "mime:1", refs[0].SourcePartKey)
	var count int
	require.NoError(t, f.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, id).Scan(&count))
	assert.Equal(t, 2, count)
}
