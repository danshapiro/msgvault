package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// Importer handles importing WhatsApp messages into the msgvault store.
type Importer struct {
	store    *store.Store
	progress ImportProgress
}

// NewImporter creates a new WhatsApp importer.
func NewImporter(s *store.Store, progress ImportProgress) *Importer {
	if progress == nil {
		progress = NullProgress{}
	}
	return &Importer{
		store:    s,
		progress: progress,
	}
}

// Import detects the WhatsApp database schema and imports its messages.
func (imp *Importer) Import(ctx context.Context, waDBPath string, opts ImportOptions) (result *ImportSummary, retErr error) {
	startTime := time.Now()
	summary := &ImportSummary{GroupRoster: "complete", ErrorCounts: make(map[string]int64), SkippedCounts: make(map[string]int64)}
	reportError := func(category string, err error) {
		summary.Errors++
		summary.ErrorCounts[category]++
		if strings.HasPrefix(category, "group_roster") && summary.GroupRoster == "complete" {
			summary.GroupRoster = "partial"
		}
		imp.progress.OnError(err)
	}

	waDB, err := openReadOnlyDatabase(ctx, waDBPath)
	if err != nil {
		return nil, fmt.Errorf("open whatsapp db: %w", err)
	}
	defer func() { _ = waDB.Close() }()

	kind, err := detectDatabaseKind(ctx, waDB)
	if err != nil {
		return nil, err
	}
	if kind == databaseKindApple {
		return imp.importApple(ctx, waDB, waDBPath, opts)
	}
	if !opts.After.IsZero() || !opts.Before.IsZero() {
		return nil, errors.New("--after and --before are only supported for Apple ChatStorage.sqlite imports")
	}

	// Keep the detailed Android schema checks for compatibility with the
	// existing importer error handling.
	if err := verifyWhatsAppDB(ctx, waDB); err != nil {
		return nil, err
	}

	// Create or get the WhatsApp source.
	source, err := imp.store.GetOrCreateSourceContext(ctx, "whatsapp", opts.Phone)
	if err != nil {
		return nil, fmt.Errorf("get or create source: %w", err)
	}

	if opts.DisplayName != "" {
		if err := imp.store.UpdateSourceDisplayNameContext(ctx, source.ID, opts.DisplayName); err != nil {
			return nil, fmt.Errorf("update source display name: %w", err)
		}
	}
	summary.SourceID = source.ID

	// Start a sync run for tracking.
	syncID, err := imp.store.StartSyncContext(ctx, source.ID, "whatsapp_import")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	imp = &scoped

	// A partially imported snapshot is never recorded as a successful sync.
	// Keep accumulated counters even when an item failed or context was cancelled.
	var syncErr error
	defer func() {
		// Record committed progress even after the caller cancels the import.
		finishCtx := context.WithoutCancel(ctx)
		cp := &store.Checkpoint{MessagesProcessed: summary.MessagesProcessed,
			MessagesAdded: summary.MessagesAdded, MessagesUpdated: summary.MessagesUpdated, ErrorsCount: summary.Errors}
		if syncErr == nil && summary.Errors > 0 {
			syncErr = fmt.Errorf("partial WhatsApp import: %d errors", summary.Errors)
		}
		var finishErr error
		if syncErr != nil {
			finishErr = imp.store.FailSyncWithCheckpointContext(finishCtx, syncID, syncErr.Error(), cp)
		} else if finishErr = imp.store.UpdateSyncCheckpointContext(finishCtx, syncID, cp); finishErr == nil {
			finishErr = imp.store.CompleteSyncContext(finishCtx, syncID, "")
		}
		if finishErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("finalize WhatsApp sync: %w", finishErr))
		}
	}()

	imp.progress.OnStart()
	if opts.Limit > 0 {
		reportError("limit", errors.New("message limit makes this import incomplete"))
	}

	// Create participant for the phone owner (self).
	selfParticipantID, err := imp.store.EnsureParticipantByPhone(ctx, opts.Phone, opts.DisplayName, "whatsapp")
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("ensure self participant: %w", err)
	}
	summary.Participants++

	// Fetch all chats from WhatsApp DB.
	chats, err := fetchChats(ctx, waDB)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fetch chats: %w", err)
	}

	// Rows outside visible chats must not disappear from accounting.
	hiddenCount, orphanCount, err := countUnprocessedMessages(ctx, waDB)
	if err != nil {
		reportError("message_coverage", err)
	} else {
		summary.MessagesProcessed += hiddenCount + orphanCount
		summary.MessagesSkipped += hiddenCount + orphanCount
		if hiddenCount > 0 {
			summary.SkippedCounts["hidden_chat"] += hiddenCount
		}
		if orphanCount > 0 {
			summary.SkippedCounts["missing_chat"] += orphanCount
			summary.Errors += orphanCount
			summary.ErrorCounts["message_orphan_chat"] += orphanCount
			imp.progress.OnError(fmt.Errorf("%d messages have no usable chat identity", orphanCount))
		}
	}

	// A group row with no JID cannot be reached by a per-chat lookup.
	if count, err := countOrphanRosterGroups(ctx, waDB); err != nil {
		reportError("group_roster_query", err)
	} else if count > 0 {
		summary.GroupRoster = "partial"
		summary.Errors += count
		summary.ErrorCounts["group_roster_orphan_group"] += count
		imp.progress.OnError(fmt.Errorf("group roster has %d entries without a corresponding group chat", count))
	}

	// Load lid → phone mapping for resolving "lid" senders.
	lidMap, err := fetchLidMap(ctx, waDB)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fetch lid map: %w", err)
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}

	// Track key_id → message_id for reply threading within each chat.
	// Scoped per chat to bound memory; cross-chat quotes won't thread
	// but that's rare and the quoted text is still in the message body.
	keyIDToMsgID := make(map[string]int64)

	totalLimit := opts.Limit
	totalAdded := int64(0)

	for _, chat := range chats {
		// Clear reply map per chat to prevent unbounded growth.
		clear(keyIDToMsgID)
		if ctx.Err() != nil {
			syncErr = ctx.Err()
			return nil, ctx.Err()
		}

		// Check global limit.
		if totalLimit > 0 && totalAdded >= int64(totalLimit) {
			break
		}

		summary.ChatsProcessed++

		// Map chat to conversation.
		sourceConvID, convType, title := mapConversation(chat)
		conversationID, err := imp.store.EnsureConversationWithTypeContext(ctx, source.ID, sourceConvID, convType, title)
		if err != nil {
			reportError("conversation", fmt.Errorf("ensure conversation %s: %w", sourceConvID, err))
			continue
		}

		imp.progress.OnChatStart(chat.RawString, chatTitle(chat), 0)

		// For direct chats: add the remote participant. Skip non-phone JIDs
		// (e.g. lid:..., broadcast) — normalizePhone returns "" for those.
		if !isGroupChat(chat) && chat.User != "" {
			phone := normalizePhone(chat.User, chat.Server)
			if chat.Server == "lid" {
				phone = resolveLidSender(sql.NullInt64{Int64: chat.JIDRowID, Valid: true}, chat.Server, lidMap)
			}
			if phone != "" {
				if participantID, err := imp.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp"); err != nil {
					reportError("participant", fmt.Errorf("ensure participant %s: %w", phone, err))
				} else {
					summary.Participants++
					if err := imp.store.EnsureConversationParticipantContext(ctx, conversationID, participantID, "member"); err != nil {
						reportError("participant", fmt.Errorf("save participant: %w", err))
					}
					if err := imp.store.EnsureConversationParticipantContext(ctx, conversationID, selfParticipantID, "member"); err != nil {
						reportError("participant", fmt.Errorf("save self participant: %w", err))
					}
				}
			}
		}

		// Group rosters in current backups use LIDs. Only mapped phone JIDs
		// or the explicit self identity are safe to add as participants.
		if isGroupChat(chat) {
			if groupRosterSchema(ctx, waDB) == "unavailable" {
				if summary.GroupRoster != "unavailable" {
					summary.GroupRoster = "unavailable"
					reportError("group_roster_schema", errors.New("group roster schema is unavailable; membership is incomplete"))
				}
			} else if members, err := fetchGroupParticipants(ctx, waDB, chat.RawString); err != nil {
				reportError("group_roster_query", fmt.Errorf("fetch group participants for %s: %w", sourceConvID, err))
			} else {
				// Resolve the complete per-chat roster before applying roles.
				// Multiple JIDs may resolve to the same participant; conflicting
				// ranks must never choose a role based on row order.
				roles := make(map[int64]string)
				conflicting := make(map[int64]bool)
				for _, member := range members {
					if member.Pending != 0 || member.Admin < 0 || member.Admin > 2 {
						reportError("group_roster_rank", errors.New("group roster has unconfirmed member or unsupported rank"))
						continue
					}
					participantID := selfParticipantID
					if member.MemberServer != "lid_me" {
						phone := normalizePhone(member.MemberUser, member.MemberServer)
						if phone == "" {
							reportError("group_roster_unresolved", errors.New("group member identity cannot be resolved to a phone number"))
							continue
						}
						participantID, err = imp.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
						if err != nil {
							reportError("group_roster_write", fmt.Errorf("ensure group participant: %w", err))
							continue
						}
					}
					summary.Participants++
					role := mapGroupRole(member.Admin)
					if conflicting[participantID] {
						continue
					}
					if previous, found := roles[participantID]; found && previous != role {
						delete(roles, participantID)
						conflicting[participantID] = true
						reportError("group_roster_conflicting_role", errors.New("group roster has conflicting roles for a resolved participant"))
						continue
					}
					roles[participantID] = role
				}
				for participantID, role := range roles {
					if err := imp.store.SetConversationParticipantRoleContext(ctx, conversationID, participantID, role); err != nil {
						reportError("group_roster_write", fmt.Errorf("save group participant: %w", err))
					}
				}
			}
		}

		// Track resolved sender participant IDs for participant fallback.
		// After the message loop, we ensure each sender is a conversation
		// participant — covers groups where group_participants is empty.
		chatSenderIDs := make(map[int64]struct{})

		// Process messages in batches.
		chatAdded := int64(0)
		afterID := int64(0)

		for {
			if ctx.Err() != nil {
				syncErr = ctx.Err()
				return nil, ctx.Err()
			}

			// Check global limit for this batch.
			remaining := batchSize
			if totalLimit > 0 {
				left := int64(totalLimit) - totalAdded
				if left <= 0 {
					break
				}
				if left < int64(remaining) {
					remaining = int(left)
				}
			}

			messages, err := fetchMessages(ctx, waDB, chat.RowID, afterID, remaining)
			if err != nil {
				reportError("message", fmt.Errorf("fetch messages for chat %s after %d: %w", sourceConvID, afterID, err))
				break
			}

			if len(messages) == 0 {
				break
			}

			// Collect message row IDs for batch media/reaction/quote lookups.
			msgRowIDs := make([]int64, len(messages))
			for i, m := range messages {
				msgRowIDs[i] = m.RowID
			}

			// Batch-fetch media, reactions, and quotes.
			mediaMap, err := fetchMedia(ctx, waDB, msgRowIDs)
			if err != nil {
				reportError("media", fmt.Errorf("fetch media: %w", err))
				mediaMap = make(map[int64]waMedia)
			}

			reactionMap, err := fetchReactions(ctx, waDB, msgRowIDs)
			if err != nil {
				reportError("reaction", fmt.Errorf("fetch reactions: %w", err))
				reactionMap = make(map[int64][]waReaction)
			}

			quotedMap, err := fetchQuotedMessages(ctx, waDB, msgRowIDs)
			if err != nil {
				reportError("reply", fmt.Errorf("fetch quoted messages: %w", err))
				quotedMap = make(map[int64]waQuoted)
			}

			for _, waMsg := range messages {
				summary.MessagesProcessed++
				afterID = waMsg.RowID

				// Skip system messages and calls.
				if isSkippedType(waMsg.MessageType) {
					summary.MessagesSkipped++
					summary.SkippedCounts[fmt.Sprintf("type_%d", waMsg.MessageType)]++
					continue
				}

				// Skip messages with empty key_id — they can't be uniquely
				// identified for upsert and would collide with each other.
				if waMsg.KeyID == "" {
					summary.MessagesSkipped++
					summary.SkippedCounts["missing_key"]++
					reportError("message_key", errors.New("message is missing its unique key"))
					continue
				}

				// Resolve sender.
				var senderID sql.NullInt64
				if waMsg.FromMe == 1 {
					senderID = sql.NullInt64{Int64: selfParticipantID, Valid: true}
				} else if waMsg.SenderServer.Valid && waMsg.SenderServer.String == "lid" {
					// Lid JID — resolve via jid_map before trying normalizePhone,
					// because lid user strings can be 15 digits and pass E.164
					// validation despite not being real phone numbers.
					phone := resolveLidSender(waMsg.SenderJIDRowID, waMsg.SenderServer.String, lidMap)
					if phone != "" {
						pid, err := imp.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
						if err != nil {
							reportError("participant", fmt.Errorf("ensure sender participant %s: %w", phone, err))
						} else {
							senderID = sql.NullInt64{Int64: pid, Valid: true}
						}
					} else {
						reportError("sender_unresolved", errors.New("message sender LID cannot be resolved to a phone number"))
					}
				} else if waMsg.SenderUser.Valid && waMsg.SenderUser.String != "" {
					phone := normalizePhone(waMsg.SenderUser.String, waMsg.SenderServer.String)
					if phone != "" {
						pid, err := imp.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
						if err != nil {
							reportError("participant", fmt.Errorf("ensure sender participant %s: %w", phone, err))
						} else {
							senderID = sql.NullInt64{Int64: pid, Valid: true}
						}
					} else {
						reportError("sender_unresolved", errors.New("message sender identity is not a recognized phone JID"))
					}
				} else if !isGroupChat(chat) && waMsg.FromMe == 0 {
					// In a direct chat, the other person is the sender.
					phone := normalizePhone(chat.User, chat.Server)
					if chat.Server == "lid" {
						phone = resolveLidSender(sql.NullInt64{Int64: chat.JIDRowID, Valid: true}, chat.Server, lidMap)
					}
					if phone != "" {
						pid, err := imp.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
						if err == nil {
							senderID = sql.NullInt64{Int64: pid, Valid: true}
						} else {
							reportError("participant", fmt.Errorf("ensure direct sender: %w", err))
						}
					} else {
						reportError("sender_unresolved", errors.New("direct message sender cannot be resolved"))
					}
				} else {
					reportError("sender_unresolved", errors.New("group message sender identity is missing"))
				}

				// Track sender for participant fallback.
				if senderID.Valid {
					chatSenderIDs[senderID.Int64] = struct{}{}
				}

				// Determine whether this id is already present so the sync counters
				// distinguish newly archived messages from reimported updates.
				existingMessageID, err := imp.lookupMessageByKeyID(ctx, source.ID, waMsg.KeyID)
				if err != nil {
					reportError("message", fmt.Errorf("check existing message: %w", err))
					continue
				}
				// Build and upsert the message.
				msg := mapMessage(waMsg, conversationID, source.ID, senderID)
				var messageID int64
				if senderID.Valid {
					messageID, err = imp.store.UpsertMessageContext(ctx, &msg)
				} else {
					messageID, err = imp.store.UpsertMessagePreservingAttributionContext(ctx, &msg)
				}
				if err != nil {
					reportError("message", fmt.Errorf("upsert message %s: %w", waMsg.KeyID, err))
					continue
				}

				// Track for reply threading.
				keyIDToMsgID[waMsg.KeyID] = messageID

				if existingMessageID == 0 {
					summary.MessagesAdded++
				} else {
					summary.MessagesUpdated++
				}
				chatAdded++
				totalAdded++

				// Body updates can invalidate FTS, so capture the prior sender
				// before writing if this source no longer resolves its identity.
				var priorSearchSender string
				var priorSearchErr error
				if !senderID.Valid && existingMessageID != 0 {
					priorSearchSender, priorSearchErr = imp.store.PreviousFTSSenderContext(ctx, messageID)
					if priorSearchErr != nil {
						reportError("search_index", fmt.Errorf("preserve previous search sender: %w", priorSearchErr))
					}
				}

				// Store message body.
				bodyText := sql.NullString{}
				if waMsg.TextData.Valid && waMsg.TextData.String != "" {
					bodyText = waMsg.TextData
				}
				// Check for media caption as additional body text.
				if media, ok := mediaMap[waMsg.RowID]; ok {
					if media.MediaCaption.Valid && media.MediaCaption.String != "" {
						if bodyText.Valid && bodyText.String != "" {
							// Append caption to body.
							bodyText.String += "\n\n" + media.MediaCaption.String
						} else {
							bodyText = media.MediaCaption
						}
					}
				}
				if bodyText.Valid {
					if err := imp.store.UpsertMessageBodyContext(ctx, messageID, bodyText, sql.NullString{}); err != nil {
						reportError("message_body", fmt.Errorf("save message body: %w", err))
					}
				}

				// Store raw JSON for re-parsing.
				rawJSON, err := json.Marshal(waMsg, json.Deterministic(true))
				if err != nil {
					reportError("message_raw", fmt.Errorf("encode raw message: %w", err))
				} else if err := imp.store.UpsertMessageRawWithFormatContext(ctx, messageID, rawJSON, "whatsapp_json"); err != nil {
					reportError("message_raw", fmt.Errorf("save raw message: %w", err))
				}

				// Handle media/attachments.
				if media, ok := mediaMap[waMsg.RowID]; ok {
					summary.AttachmentsFound++
					mediaType := mapMediaType(waMsg.MessageType)

					storagePath, contentHash, storedSize := imp.handleMediaFile(ctx, media, opts)
					if storagePath != "" {
						summary.MediaCopied++
					}

					mimeType := ""
					if media.MimeType.Valid {
						mimeType = media.MimeType.String
					}

					filename := ""
					if media.FilePath.Valid {
						filename = filepath.Base(media.FilePath.String)
					}

					size := int64(0)
					if storagePath != "" {
						size = storedSize
					} else if media.FileSize.Valid {
						size = media.FileSize.Int64
					}

					// Only insert attachment row when we have actual content.
					// Without --media-dir, storagePath and contentHash are both
					// empty; inserting would create broken records.
					if storagePath != "" || contentHash != "" {
						role := store.AttachmentRoleStandalone
						roleSource := store.AttachmentRoleSourceImporterSemantics
						if mediaType == "sticker" {
							role = store.AttachmentRoleSticker
							roleSource = store.AttachmentRoleSourceProviderExplicit
						}
						err := imp.store.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{
							Filename: filename, MIMEType: mimeType, StoragePath: storagePath,
							ContentHash: contentHash, Size: size, MediaType: mediaType,
							Role: role, RoleSource: roleSource, SourcePartKey: "whatsapp:media",
						})
						if err != nil {
							reportError("attachment", fmt.Errorf("upsert attachment for message %s: %w", waMsg.KeyID, err))
						}
					} else {
						// No media stored — clear attachment flags set by mapMessage
						// so Parquet/TUI queries don't show phantom attachments.
						// Only clear if the message has no pre-existing attachment rows
						// (a previous import with --media-dir may have created them).
						var existingCount int
						countErr := imp.store.DB().QueryRowContext(
							ctx,
							imp.store.Rebind("SELECT COUNT(*) FROM attachments WHERE message_id = ?"),
							messageID,
						).Scan(&existingCount)
						if countErr != nil {
							reportError("attachment", fmt.Errorf("count attachments: %w", countErr))
						} else if existingCount == 0 {
							if err := imp.store.RecomputeMessageAttachmentStatsContext(ctx, messageID); err != nil {
								reportError("attachment", fmt.Errorf("clear missing attachment flags: %w", err))
							}
						}
					}

					// Store media metadata in the attachments table is done above.
					// For extra metadata (width, height, duration, media_type),
					// update via a direct SQL call for provider dimensions and duration.
					if mediaType != "" || (media.Width.Valid && media.Width.Int64 > 0) {
						if err := imp.updateAttachmentMetadata(ctx, messageID, contentHash, mediaType, media); err != nil {
							reportError("attachment", fmt.Errorf("update attachment metadata: %w", err))
						}
					}
				}

				// Handle quoted/reply messages.
				if quoted, ok := quotedMap[waMsg.RowID]; ok {
					if replyToMsgID, found := keyIDToMsgID[quoted.QuotedKeyID]; found {
						if err := imp.setReplyTo(ctx, messageID, replyToMsgID); err != nil {
							reportError("reply", fmt.Errorf("save reply: %w", err))
						}
					} else if dbMsgID, lookupErr := imp.lookupMessageByKeyID(ctx, source.ID, quoted.QuotedKeyID); lookupErr != nil {
						reportError("reply", fmt.Errorf("lookup reply: %w", lookupErr))
					} else if dbMsgID > 0 {
						if err := imp.setReplyTo(ctx, messageID, dbMsgID); err != nil {
							reportError("reply", fmt.Errorf("save reply: %w", err))
						}
					}
				}

				// Handle reactions.
				if reactions, ok := reactionMap[waMsg.RowID]; ok {
					for _, r := range reactions {
						reactionValue := mapReaction(r)
						if reactionValue == "" {
							continue
						}

						var reactorID int64
						if r.SenderServer.Valid && r.SenderServer.String == "lid" {
							// Lid JID — resolve via jid_map first.
							phone := resolveLidSender(r.SenderJIDRowID, r.SenderServer.String, lidMap)
							if phone == "" {
								reportError("reaction_unresolved", errors.New("reaction sender LID cannot be resolved"))
								continue
							}
							pid, err := imp.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
							if err != nil {
								reportError("reaction", fmt.Errorf("ensure reaction participant: %w", err))
								continue
							}
							reactorID = pid
						} else if r.SenderUser.Valid && r.SenderUser.String != "" {
							phone := normalizePhone(r.SenderUser.String, r.SenderServer.String)
							if phone == "" {
								reportError("reaction_unresolved", errors.New("reaction sender is not a recognized phone JID"))
								continue
							}
							pid, err := imp.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
							if err != nil {
								reportError("reaction", fmt.Errorf("ensure reaction participant: %w", err))
								continue
							}
							reactorID = pid
						} else {
							// Self reaction.
							reactorID = selfParticipantID
						}

						createdAt := time.Unix(r.Timestamp/1000, 0).UTC()
						if err := imp.store.UpsertReaction(ctx, messageID, reactorID, reactionTypeEmoji, reactionValue, createdAt); err != nil {
							reportError("reaction", fmt.Errorf("upsert reaction: %w", err))
						} else {
							summary.ReactionsAdded++
						}
					}
				}

				// FTS indexing.
				if bodyText.Valid {
					senderAddr := ""
					if waMsg.FromMe == 1 {
						senderAddr = opts.Phone
					} else if waMsg.SenderServer.Valid && waMsg.SenderServer.String == "lid" {
						senderAddr = resolveLidSender(waMsg.SenderJIDRowID, waMsg.SenderServer.String, lidMap)
					} else if waMsg.SenderUser.Valid {
						senderAddr = normalizePhone(waMsg.SenderUser.String, waMsg.SenderServer.String)
					}
					if !senderID.Valid && existingMessageID != 0 {
						senderAddr = priorSearchSender
					}
					if priorSearchErr == nil {
						if err := imp.store.UpsertFTSContext(ctx, messageID, "", bodyText.String, senderAddr, "", ""); err != nil {
							reportError("search_index", fmt.Errorf("index message: %w", err))
						}
					}
				}
			}

			// Update sync run progress counters (for monitoring, not resume).
			// Resume is not implemented yet — re-running is safe due to upsert dedup.
			if err := imp.store.UpdateSyncCheckpointContext(ctx, syncID, &store.Checkpoint{
				MessagesProcessed: summary.MessagesProcessed,
				MessagesAdded:     summary.MessagesAdded, MessagesUpdated: summary.MessagesUpdated, ErrorsCount: summary.Errors,
			}); err != nil {
				reportError("checkpoint", fmt.Errorf("save sync checkpoint: %w", err))
			}

			imp.progress.OnProgress(summary.MessagesProcessed, summary.MessagesAdded, summary.MessagesSkipped)

			// If we got fewer than requested, we've finished this chat.
			if len(messages) < remaining {
				break
			}
		}

		// Participant fallback: ensure every resolved sender is a conversation
		// participant. Covers groups where group_participants is empty (newer
		// WhatsApp versions) and any senders discovered via lid resolution.
		for pid := range chatSenderIDs {
			if err := imp.store.EnsureConversationParticipantContext(ctx, conversationID, pid, "member"); err != nil {
				reportError("participant", fmt.Errorf("save sender participant: %w", err))
			}
		}
		// Always include self as participant.
		if err := imp.store.EnsureConversationParticipantContext(ctx, conversationID, selfParticipantID, "member"); err != nil {
			reportError("participant", fmt.Errorf("save self participant: %w", err))
		}

		imp.progress.OnChatComplete(chat.RawString, chatAdded)
	}
	// Update denormalised conversation counts for the WhatsApp source. Chats
	// written before a cancel stay committed, so refresh them even when the
	// import is stopping.
	if err := imp.store.RecomputeConversationStatsContext(context.WithoutCancel(ctx), source.ID); err != nil {
		reportError("conversation_stats", fmt.Errorf("recompute conversation stats: %w", err))
	}
	// A cancel during the last chat's queries or writes ends its loops as
	// errors, past the cancellation checks above. Report it so the run is
	// not recorded as completed.
	if err := ctx.Err(); err != nil {
		syncErr = err
		return nil, err
	}

	summary.Duration = time.Since(startTime)
	imp.progress.OnComplete(summary)

	return summary, nil
}

// handleMediaFile attempts to find and copy a media file to content-addressed storage.
// Returns (storagePath, contentHash, storedSize). All are empty or zero if the
// file is not found.
func (imp *Importer) handleMediaFile(ctx context.Context, media waMedia, opts ImportOptions) (string, string, int64) {
	if opts.MediaDir == "" || opts.AttachmentsDir == "" || !media.FilePath.Valid || media.FilePath.String == "" {
		return "", "", 0
	}

	mediaDir := opts.MediaDir

	// Sanitize the path from the WhatsApp DB (untrusted data).
	relPath := filepath.Clean(media.FilePath.String)

	// Reject absolute paths — the DB should only contain relative paths.
	if filepath.IsAbs(relPath) {
		relPath = filepath.Base(relPath)
	}

	// Reject directory traversal.
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		relPath = filepath.Base(relPath)
	}

	// Build candidate path and verify it stays within mediaDir.
	fullPath := filepath.Join(mediaDir, relPath)
	absMediaDir, err := filepath.Abs(mediaDir)
	if err != nil {
		return "", "", 0
	}
	absFullPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", "", 0
	}
	if !strings.HasPrefix(absFullPath, absMediaDir+string(filepath.Separator)) && absFullPath != absMediaDir {
		// Path escapes mediaDir — fall back to base filename only.
		fullPath = filepath.Join(mediaDir, filepath.Base(relPath))
		absFullPath, _ = filepath.Abs(fullPath)
		if !strings.HasPrefix(absFullPath, absMediaDir+string(filepath.Separator)) {
			return "", "", 0
		}
	}

	// Check file exists.
	info, err := os.Stat(fullPath)
	if err != nil {
		// Try just the filename as fallback.
		fullPath = filepath.Join(mediaDir, filepath.Base(relPath))
		info, err = os.Stat(fullPath)
		if err != nil {
			return "", "", 0
		}
	}

	// Enforce max file size to prevent OOM.
	maxSize := opts.MaxMediaFileSize
	if maxSize <= 0 {
		maxSize = 100 * 1024 * 1024 // 100MB default
	}
	if info.Size() > maxSize {
		return "", "", 0
	}

	relStoragePath, contentHash, storedSize, err := export.StoreAttachmentFromPath(
		ctx,
		opts.AttachmentsDir, fullPath, maxSize)
	if err != nil {
		// contentHash is non-empty when hashing succeeded but storage failed;
		// callers use it to attach metadata to the unstored attachment row.
		return "", contentHash, 0
	}
	return relStoragePath, contentHash, storedSize
}

// updateAttachmentMetadata updates media-specific metadata on an attachment record.
func (imp *Importer) updateAttachmentMetadata(
	ctx context.Context, messageID int64, contentHash, mediaType string, media waMedia,
) error {
	var width, height, durationMS sql.NullInt64
	if media.Width.Valid && media.Width.Int64 > 0 {
		width = media.Width
	}
	if media.Height.Valid && media.Height.Int64 > 0 {
		height = media.Height
	}
	if media.MediaDuration.Valid && media.MediaDuration.Int64 > 0 {
		// WhatsApp stores duration in seconds; msgvault uses milliseconds.
		durationMS = sql.NullInt64{Int64: media.MediaDuration.Int64 * 1000, Valid: true}
	}

	return imp.store.UpdateAttachmentMediaMetadataContext(
		ctx, messageID, contentHash, mediaType, width, height, durationMS)
}

// lookupMessageByKeyID looks up a previously imported message by its WhatsApp key_id.
// Returns 0 if not found.
func (imp *Importer) lookupMessageByKeyID(ctx context.Context, sourceID int64, keyID string) (int64, error) {
	var msgID int64
	err := imp.store.DB().QueryRowContext(
		ctx,
		imp.store.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
		sourceID, keyID,
	).Scan(&msgID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return msgID, err
}

// setReplyTo sets the reply_to_message_id on a message.
func (imp *Importer) setReplyTo(ctx context.Context, messageID, replyToID int64) error {
	return imp.store.SetMessageReplyContext(ctx, messageID, replyToID)
}

// verifyWhatsAppDB checks that the database looks like a WhatsApp msgstore.db.
func verifyWhatsAppDB(ctx context.Context, db *sql.DB) error {
	// Check for the 'message' table with expected columns.
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'message'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check whatsapp db: %w", err)
	}
	if count == 0 {
		return errors.New("not a valid WhatsApp database: 'message' table not found")
	}

	// Check for the 'jid' table.
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'jid'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check whatsapp db: %w", err)
	}
	if count == 0 {
		return errors.New("not a valid WhatsApp database: 'jid' table not found")
	}

	// Check for the 'chat' table.
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'chat'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check whatsapp db: %w", err)
	}
	if count == 0 {
		return errors.New("not a valid WhatsApp database: 'chat' table not found")
	}

	// These columns are indispensable to the message and conversation queries.
	// Detect schema drift before starting a sync or mutating the destination.
	for table, columns := range map[string][]string{
		"jid":     {"_id", "user", "server", "raw_string"},
		"chat":    {"_id", "jid_row_id", "hidden", "subject", "sort_timestamp"},
		"message": {"_id", "chat_row_id", "from_me", "key_id", "sender_jid_row_id", "timestamp", "message_type", "text_data", "status", "starred"},
	} {
		for _, column := range columns {
			if !hasColumn(ctx, db, table, column) {
				return fmt.Errorf("unsupported WhatsApp schema: missing %s.%s", table, column)
			}
		}
	}

	return nil
}
