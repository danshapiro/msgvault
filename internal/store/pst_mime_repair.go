package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/textutil"
)

// PSTRepairBody describes the old and corrected MIME-derived body. The stored
// body is changed only when it still matches the old derived value.
type PSTRepairBody struct {
	ExpectedText, ExpectedHTML string
	NewText, NewHTML           string
}

type PSTRepairStoreResult struct {
	AttachmentsAdded int
	BodyUpdated      bool
	BodyPreserved    bool
}

// RepairPSTRawAndAttachments changes one identified raw MIME snapshot using
// a compare-and-swap and adds missing MIME attachment occurrences. Existing
// attachment data, message identity, labels and thread information are never
// replaced. An empty attachment location may be filled when its hash matches.
// Files must already have been stored using the normal attachment
// storage path; a failed file can be retried on a subsequent invocation.
func (s *Store) RepairPSTRawAndAttachments(ctx context.Context, sourceID, messageID int64, sourceMessageID string, expectedRaw, replacementRaw []byte, writes []AttachmentWrite, body *PSTRepairBody) (PSTRepairStoreResult, error) {
	var result PSTRepairStoreResult
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockMeetingEvidenceWith(ctx, tx, messageID); err != nil {
			return fmt.Errorf("lock PST message: %w", err)
		}
		q := boundQuerier{ctx: ctx, q: tx}
		var encoded []byte
		var compression sql.NullString
		err := q.QueryRow(`SELECT r.raw_data, r.compression FROM messages m
			JOIN message_raw r ON r.message_id = m.id
			WHERE m.id = ? AND m.source_id = ? AND m.source_message_id = ? AND r.raw_format = 'mime'`+s.dialect.SelectForUpdate(), messageID, sourceID, sourceMessageID).Scan(&encoded, &compression)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("PST message identity or MIME changed")
		}
		if err != nil {
			return fmt.Errorf("read PST message MIME: %w", err)
		}
		current, err := decodeMessageRaw(encoded, compression)
		if err != nil {
			return fmt.Errorf("decode PST message MIME: %w", err)
		}
		if !bytes.Equal(current, expectedRaw) {
			return errors.New("PST message MIME changed during repair")
		}
		added, err := s.addPSTRepairAttachments(ctx, tx, messageID, writes)
		if err != nil {
			return err
		}
		result.AttachmentsAdded = added
		changedRaw := !bytes.Equal(current, replacementRaw)
		if changedRaw {
			if err := upsertMessageRawWithFormat(q, messageID, replacementRaw, "mime"); err != nil {
				return fmt.Errorf("update PST message MIME: %w", err)
			}
		}
		if body != nil {
			var oldText, oldHTML sql.NullString
			err := q.QueryRow(`SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`, messageID).Scan(&oldText, &oldHTML)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("read PST message body: %w", err)
			}
			if oldText.String == body.ExpectedText && oldHTML.String == body.ExpectedHTML {
				if oldText.String != body.NewText || oldHTML.String != body.NewHTML {
					text := sql.NullString{String: body.NewText, Valid: body.NewText != ""}
					html := sql.NullString{String: body.NewHTML, Valid: body.NewHTML != ""}
					if err := upsertMessageBody(q, s.dialect, s.fts5Available, messageID, text, html); err != nil {
						return fmt.Errorf("update PST message body: %w", err)
					}
					// Refresh only the snippet generated from the same old body;
					// a separately changed preview is existing user data.
					_, err := q.Exec(`UPDATE messages SET snippet = ? WHERE id = ? AND COALESCE(snippet, '') = ?`,
						nullIfEmpty(textutil.SnippetFromBody(body.NewText)), messageID, textutil.SnippetFromBody(body.ExpectedText))
					if err != nil {
						return fmt.Errorf("update PST message snippet: %w", err)
					}
					if s.fts5Available {
						if _, err := q.Exec(s.dialect.FTSBackfillBatchSQL(), messageID, messageID+1); err != nil {
							return fmt.Errorf("update PST message search: %w", err)
						}
					}
					result.BodyUpdated = true
				}
			} else if oldText.String != body.NewText || oldHTML.String != body.NewHTML {
				result.BodyPreserved = true
			}
		}
		if added > 0 {
			if err := recomputeMessageAttachmentStatsWith(q, messageID); err != nil {
				return fmt.Errorf("update PST attachment counts: %w", err)
			}
		}
		if changedRaw || added > 0 || result.BodyUpdated {
			if err := s.bumpDerivedDataRevision(tx); err != nil {
				return fmt.Errorf("update PST repair revision: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return PSTRepairStoreResult{}, err
	}
	return result, nil
}

type pstRepairExistingAttachment struct {
	id                      int64
	key, hash, path, source string
	filename, contentID     string
	state                   string
	size                    int64
	used                    bool
}

func (s *Store) addPSTRepairAttachments(ctx context.Context, tx *loggedTx, messageID int64, writes []AttachmentWrite) (int, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(source_part_key, ''), COALESCE(content_hash, ''),
		COALESCE(storage_path, ''), COALESCE(source_attachment_id, ''), COALESCE(filename, ''),
		COALESCE(content_id, ''), COALESCE(size, 0), COALESCE(attachment_state, '')
		FROM attachments WHERE message_id = ? AND source_attachment_id IS NULL
		AND NOT EXISTS (SELECT 1 FROM source_merge_preserved_attachments spa WHERE spa.attachment_id = attachments.id)
		ORDER BY id`+s.dialect.SelectForUpdate(), messageID)
	if err != nil {
		return 0, fmt.Errorf("read existing PST attachments: %w", err)
	}
	var existing []pstRepairExistingAttachment
	for rows.Next() {
		var item pstRepairExistingAttachment
		if err := rows.Scan(&item.id, &item.key, &item.hash, &item.path, &item.source, &item.filename, &item.contentID, &item.size, &item.state); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("read PST attachment: %w", err)
		}
		existing = append(existing, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read PST attachments: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	added := 0
	matchExisting := func(item *pstRepairExistingAttachment, write AttachmentWrite) error {
		if item.used || item.source != "" || !strings.EqualFold(item.hash, write.ContentHash) {
			return fmt.Errorf("existing PST attachment conflicts with MIME part %q", write.SourcePartKey)
		}
		if item.state != "" && item.state != "stored" {
			return fmt.Errorf("existing PST attachment %q has state %q", write.SourcePartKey, item.state)
		}
		if item.path == "" {
			// Fill an empty location only; all existing metadata and identities
			// remain untouched. The new blob was verified before this transaction.
			res, err := q.Exec(`UPDATE attachments SET storage_path = ? WHERE id = ? AND COALESCE(storage_path, '') = '' AND content_hash = ?`, write.StoragePath, item.id, item.hash)
			if err != nil {
				return fmt.Errorf("restore PST attachment %q location: %w", write.SourcePartKey, err)
			}
			count, err := res.RowsAffected()
			if err != nil || count != 1 {
				return fmt.Errorf("PST attachment %q changed during repair", write.SourcePartKey)
			}
			added++
		} else if item.path != write.StoragePath {
			return fmt.Errorf("existing PST attachment %q has a different storage location", write.SourcePartKey)
		}
		item.used = true
		return nil
	}
	for _, write := range writes {
		write = write.normalized()
		if err := write.validate(); err != nil {
			return 0, err
		}
		if write.SourcePartKey == "" || write.ContentHash == "" || write.StoragePath == "" || write.SourceAttachmentID != "" {
			return 0, errors.New("PST repair attachment lacks a stored MIME part identity")
		}
		matched := false
		for i := range existing {
			item := &existing[i]
			if item.key != write.SourcePartKey {
				continue
			}
			if err := matchExisting(item, write); err != nil {
				return 0, err
			}
			matched = true
			break
		}
		if !matched {
			for i := range existing {
				item := &existing[i]
				if !item.used && item.key == "" && item.source == "" && strings.EqualFold(item.hash, write.ContentHash) && item.filename == write.Filename && item.contentID == write.ContentID && item.size == write.Size {
					if err := matchExisting(item, write); err != nil {
						return 0, err
					}
					matched = true
					break
				}
			}
		}
		if matched {
			continue
		}
		// Never update an existing row: a concurrent or conflicting occurrence
		// must fail the repair rather than replace archived attachment data.
		res, err := q.Exec(`INSERT INTO attachments
			(message_id, filename, mime_type, storage_path, content_hash, size, attachment_role, role_source, source_part_key, content_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+s.dialect.Now()+`) ON CONFLICT DO NOTHING`,
			messageID, write.Filename, write.MIMEType, write.StoragePath, write.ContentHash, write.Size,
			string(write.Role), string(write.RoleSource), write.SourcePartKey, nullIfEmpty(write.ContentID))
		if err != nil {
			return 0, fmt.Errorf("insert PST attachment %q: %w", write.SourcePartKey, err)
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return 0, fmt.Errorf("PST attachment %q changed during repair", write.SourcePartKey)
		}
		added++
	}
	return added, nil
}
