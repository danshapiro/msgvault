package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jhillyerd/enmime/v2"
	"go.kenn.io/msgvault/internal/export"
	msgmime "go.kenn.io/msgvault/internal/mime"
	pstreader "go.kenn.io/msgvault/internal/pst"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// PSTRepairOptions identifies exactly one existing imported message.
type PSTRepairOptions struct {
	SourceID       int64
	ArchivePath    string
	ArchiveID      string
	EntryID        string
	AttachmentsDir string
}

// PSTRepairResult reports successful changes even if an attachment was blocked.
type PSTRepairResult struct {
	MessageID         int64
	SourceMessageID   string
	ArchiveID         string
	AttachmentsAdded  int
	BodyUpdated       bool
	BodyPreserved     bool
	EnvelopeUpdated   bool
	EnvelopePreserved []string
	AttachmentErrors  []error
	Warnings          []string
}

// RepairPSTMessage repairs exactly one stored PST message. The source archive
// is only read; the currently stored MIME body remains byte-for-byte intact.
func RepairPSTMessage(ctx context.Context, st *store.Store, opts PSTRepairOptions, log *slog.Logger) (PSTRepairResult, error) {
	var result PSTRepairResult
	if opts.SourceID <= 0 || opts.ArchivePath == "" || opts.ArchiveID == "" || opts.EntryID == "" || opts.AttachmentsDir == "" {
		return result, errors.New("source, archive path and fingerprint, entry ID, and attachment directory are required")
	}
	id, err := strconv.ParseInt(opts.EntryID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != opts.EntryID {
		return result, errors.New("PST entry ID must be a positive decimal identifier")
	}
	src, err := st.GetSourceByIDContext(ctx, opts.SourceID)
	if err != nil {
		return result, err
	}
	if src.SourceType != "pst" {
		return result, errors.New("source is not a PST source")
	}
	archiveID, err := pstArchiveFingerprint(opts.ArchivePath)
	if err != nil {
		return result, fmt.Errorf("fingerprint PST: %w", err)
	}
	if archiveID != opts.ArchiveID {
		return result, fmt.Errorf("PST archive fingerprint mismatch: got %s", archiveID)
	}
	result.ArchiveID = archiveID
	result.SourceMessageID = fmt.Sprintf("pst-%s-%s", archiveID, opts.EntryID)
	ids, err := st.MessageExistsWithRawBatchContext(ctx, opts.SourceID, []string{result.SourceMessageID})
	if err != nil {
		return result, fmt.Errorf("find stored PST message: %w", err)
	}
	result.MessageID = ids[result.SourceMessageID]
	if result.MessageID == 0 {
		return result, errors.New("stored PST message with raw MIME not found")
	}
	file, err := pstreader.Open(opts.ArchivePath)
	if err != nil {
		return result, err
	}
	defer func() { _ = file.Close() }()
	initialInfo, err := file.Stat()
	if err != nil {
		return result, fmt.Errorf("stat open PST: %w", err)
	}
	openedArchiveID, err := file.ArchiveFingerprint()
	if err != nil {
		return result, fmt.Errorf("fingerprint open PST: %w", err)
	}
	if openedArchiveID != archiveID {
		return result, errors.New("PST archive changed before entry lookup")
	}
	message, err := file.GetMessage(id)
	if err != nil {
		return result, fmt.Errorf("read PST entry: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	entry := pstreader.ExtractMessage(message, "")
	if entry == nil || entry.EntryID != opts.EntryID {
		return result, errors.New("PST entry is not the requested mail message")
	}
	if entry.HeaderError != nil {
		return result, fmt.Errorf("read PST header properties: %w", entry.HeaderError)
	}
	entry.PSTArchiveID = archiveID
	verifySource := func() error {
		currentInfo, err := file.Stat()
		if err != nil {
			return fmt.Errorf("stat open PST: %w", err)
		}
		pathInfo, err := os.Stat(opts.ArchivePath)
		if err != nil {
			return fmt.Errorf("stat PST path: %w", err)
		}
		if !os.SameFile(initialInfo, pathInfo) || currentInfo.Size() != initialInfo.Size() || !currentInfo.ModTime().Equal(initialInfo.ModTime()) {
			return errors.New("PST source changed during repair")
		}
		currentID, err := file.ArchiveFingerprint()
		if err != nil || currentID != archiveID {
			return errors.New("PST archive fingerprint changed during repair")
		}
		return nil
	}
	return repairPSTMessageWithVerify(ctx, st, opts.SourceID, result.SourceMessageID, result.MessageID, entry, opts.AttachmentsDir, log, verifySource)
}

func repairPSTMessage(ctx context.Context, st *store.Store, sourceID int64, sourceMessageID string, messageID int64, entry *pstreader.MessageEntry, attachmentsDir string, log *slog.Logger) (PSTRepairResult, error) {
	return repairPSTMessageWithVerify(ctx, st, sourceID, sourceMessageID, messageID, entry, attachmentsDir, log, nil)
}

func repairPSTMessageWithVerify(ctx context.Context, st *store.Store, sourceID int64, sourceMessageID string, messageID int64, entry *pstreader.MessageEntry, attachmentsDir string, log *slog.Logger, verifySource func() error) (PSTRepairResult, error) {
	result := PSTRepairResult{MessageID: messageID, SourceMessageID: sourceMessageID, ArchiveID: entry.PSTArchiveID}
	if attachmentsDir == "" {
		return result, errors.New("attachment directory is required")
	}
	oldRaw, err := st.GetMessageRawContext(ctx, messageID)
	if err != nil {
		return result, fmt.Errorf("read stored PST message: %w", err)
	}
	newRaw, _, err := pstreader.RepairTransportHeader(entry, oldRaw)
	if err != nil {
		return result, fmt.Errorf("repair PST transport header: %w", err)
	}
	oldMessage, _ := msgmime.ParseWithRecovery(oldRaw, "(MIME parse error)")
	parsed, err := msgmime.Parse(newRaw)
	if err != nil {
		return result, fmt.Errorf("parse repaired PST MIME: %w", err)
	}
	var blocking []string
	uncertainBody := false
	for _, issue := range parsed.Issues {
		if issue.Name != enmime.ErrorCharsetDeclaration || issue.Severe || (!issue.BodyPart && !issue.UnconvertedAttachment) {
			blocking = append(blocking, issue.Description)
			continue
		}
		if issue.BodyPart && !issue.UTF8Restored {
			uncertainBody = true
		}
		warning := issue.Description
		if issue.UnconvertedAttachment {
			warning += "; attachment bytes retained without charset conversion"
		} else if !issue.UTF8Restored {
			warning += "; body encoding is uncertain, so the existing body will be preserved if repair succeeds"
		}
		result.Warnings = append(result.Warnings, warning)
		if log != nil {
			log.Warn("PST repair MIME charset detection warning", "message", messageID, "warning", issue.Description, "body_preserved", issue.BodyPart && !issue.UTF8Restored)
		}
	}
	if len(blocking) != 0 {
		return result, fmt.Errorf("parse repaired PST MIME parts: %s", strings.Join(blocking, "; "))
	}
	var writes []store.AttachmentWrite
	for i := range parsed.Attachments {
		att := &parsed.Attachments[i]
		if err := ctx.Err(); err != nil {
			return result, err
		}
		path, err := export.StoreAttachmentFileIncludingEmpty(attachmentsDir, att)
		if err == nil && path == "" {
			err = errors.New("attachment content unavailable")
		}
		if err != nil {
			failure := fmt.Errorf("store attachment %q: %w", att.Filename, err)
			result.AttachmentErrors = append(result.AttachmentErrors, failure)
			if log != nil {
				log.Warn("PST repair attachment blocked", "message", messageID, "filename", att.Filename, "error", err)
			}
			continue
		}
		role, roleSource := store.AttachmentRoleFromMIME(att.Disposition, att.IsInline, att.ContentID)
		writes = append(writes, store.AttachmentWrite{Filename: att.Filename, MIMEType: att.ContentType, StoragePath: path,
			ContentHash: att.ContentHash, Size: int64(len(att.Content)), Role: role, RoleSource: roleSource,
			SourcePartKey: att.PartKey, ContentID: att.ContentID})
	}
	body := &store.PSTRepairBody{
		ExpectedText: textutil.EnsureUTF8(oldMessage.GetBodyText()), ExpectedHTML: textutil.EnsureUTF8(oldMessage.BodyHTML),
		NewText: textutil.EnsureUTF8(parsed.GetBodyText()), NewHTML: textutil.EnsureUTF8(parsed.BodyHTML),
	}
	if uncertainBody {
		// A charset detector's guess is insufficient evidence to replace an
		// existing body. The repaired raw MIME remains available for recovery.
		body = nil
	}
	now := time.Now().UTC()
	fallbackDate := entry.FallbackDate(now)
	oldDate, _ := msgmime.ResolveMessageDate(oldMessage.Date, oldMessage.ReceivedDates, fallbackDate, now)
	newDate, _ := msgmime.ResolveMessageDate(parsed.Date, parsed.ReceivedDates, fallbackDate, now)
	rfcID, parent, refs := msgmime.ParseThreadingHeaders(newRaw)
	threadKey := rfcID
	if parent != "" {
		threadKey = parent
	}
	if len(refs) != 0 {
		if root := msgmime.NormalizeMessageID(refs[0]); root != "" {
			threadKey = root
		}
	}
	envelope := &store.PSTRepairEnvelope{
		ExpectedSubject: textutil.EnsureUTF8(oldMessage.Subject), Subject: textutil.EnsureUTF8(parsed.Subject),
		ExpectedSentAt: oldDate, SentAt: newDate,
		From: parsed.From, To: parsed.To, Cc: parsed.Cc, Bcc: parsed.Bcc,
		MessageID: rfcID, InReplyTo: parent, ThreadKey: threadKey, UnresolvedRecipients: unresolvedPSTRecipientRoles(newRaw),
	}
	if unresolved := unresolvedPSTSender(newRaw); unresolved != nil {
		envelope.UnresolvedSenderName = unresolved.Name
		envelope.UnresolvedSenderKey = unresolved.participantKey(sourceID, sourceMessageID)
		envelope.UnresolvedSender = true
		for _, from := range parsed.From {
			if strings.Contains(from.Email, "@") {
				envelope.UnresolvedSender = false
				break
			}
		}
	}
	if verifySource != nil {
		if err := verifySource(); err != nil {
			return result, errors.Join(fmt.Errorf("verify PST source: %w", err), errors.Join(result.AttachmentErrors...))
		}
	}
	stored, err := st.RepairPSTRawAndAttachments(ctx, sourceID, messageID, sourceMessageID, oldRaw, newRaw, writes, body, envelope)
	if err != nil {
		return result, errors.Join(fmt.Errorf("commit PST message repair: %w", err), errors.Join(result.AttachmentErrors...))
	}
	result.AttachmentsAdded, result.BodyUpdated, result.BodyPreserved = stored.AttachmentsAdded, stored.BodyUpdated, stored.BodyPreserved
	result.BodyPreserved = result.BodyPreserved || uncertainBody
	result.EnvelopeUpdated, result.EnvelopePreserved = stored.EnvelopeUpdated, stored.EnvelopePreserved
	if len(result.AttachmentErrors) != 0 {
		return result, fmt.Errorf("PST message repaired with blocked attachments: %w", errors.Join(result.AttachmentErrors...))
	}
	if stored.BodyPreserved && log != nil {
		log.Warn("PST repair preserved a body that differs from the original MIME-derived body", "message", messageID)
	}
	return result, nil
}
