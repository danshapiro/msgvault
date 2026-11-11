package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// hasColumn checks whether a table has a given column using PRAGMA table_info.
// Results are cached per (db pointer, table) to avoid repeated PRAGMA queries
// while correctly handling multiple databases with different schemas.
var (
	columnCache   = make(map[columnCacheKey]map[string]bool)
	columnCacheMu sync.Mutex
)

type columnCacheKey struct {
	db    *sql.DB
	table string
}

func hasColumn(ctx context.Context, db *sql.DB, table, column string) bool {
	columnCacheMu.Lock()
	defer columnCacheMu.Unlock()

	key := columnCacheKey{db: db, table: table}
	cols, ok := columnCache[key]
	if !ok {
		cols = make(map[string]bool)
		rows, err := db.QueryContext(
			ctx,
			fmt.Sprintf("PRAGMA table_info(%s)", table),
		)
		if err == nil {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var cid int
				var name, colType string
				var notNull, pk int
				var dfltValue sql.NullString
				if err := rows.Scan(
					&cid, &name, &colType,
					&notNull, &dfltValue, &pk,
				); err == nil {
					cols[name] = true
				}
			}
			err = rows.Err()
		}
		// Only cache a fully-read column set; on query or iteration error
		// re-probe next time rather than memoizing a partial result.
		if err == nil {
			columnCache[key] = cols
		}
	}
	return cols[column]
}

// resetColumnCache clears the cached column info (for testing).
func resetColumnCache() {
	columnCacheMu.Lock()
	defer columnCacheMu.Unlock()
	columnCache = make(map[columnCacheKey]map[string]bool)
}

// fetchChats returns all non-hidden chats from the WhatsApp database.
// Joins with the jid table to get JID details for each chat.
// Handles old WhatsApp schemas that lack the group_type column.
func fetchChats(ctx context.Context, db *sql.DB) ([]waChat, error) {
	hasGroupType := hasColumn(ctx, db, "chat", "group_type")

	groupTypeExpr := "0"
	if hasGroupType {
		groupTypeExpr = "COALESCE(c.group_type, 0)"
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			c._id,
			c.jid_row_id,
			j.raw_string,
			COALESCE(j.user, ''),
			COALESCE(j.server, ''),
			c.subject,
			%s,
			COALESCE(c.hidden, 0),
			COALESCE(c.sort_timestamp, 0)
		FROM chat c
		JOIN jid j ON c.jid_row_id = j._id
		WHERE COALESCE(c.hidden, 0) = 0
		ORDER BY c.sort_timestamp DESC
	`, groupTypeExpr))
	if err != nil {
		return nil, fmt.Errorf("fetch chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var chats []waChat
	for rows.Next() {
		var c waChat
		if err := rows.Scan(
			&c.RowID, &c.JIDRowID, &c.RawString, &c.User, &c.Server,
			&c.Subject, &c.GroupType, &c.Hidden,
			&c.LastMessageTimestamp,
		); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// countUnprocessedMessages distinguishes intentional hidden-chat exclusions
// from messages that cannot be routed to an existing chat identity.
func countUnprocessedMessages(ctx context.Context, db *sql.DB) (hidden, orphan int64, err error) {
	err = db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN c._id IS NOT NULL AND COALESCE(c.hidden, 0) <> 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN c._id IS NULL OR (COALESCE(c.hidden, 0) = 0 AND j._id IS NULL) THEN 1 ELSE 0 END), 0)
		FROM message m LEFT JOIN chat c ON c._id = m.chat_row_id
		LEFT JOIN jid j ON j._id = c.jid_row_id`).Scan(&hidden, &orphan)
	if err != nil {
		return 0, 0, fmt.Errorf("account for unprocessed messages: %w", err)
	}
	return hidden, orphan, nil
}

// Orphan system events are excluded by the same policy as events in known
// chats. Unknown types remain unresolved, even when their text is empty.
func countExcludedOrphanTypes(ctx context.Context, db *sql.DB) (map[int]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.message_type,COUNT(*)
		FROM message m LEFT JOIN chat c ON c._id=m.chat_row_id LEFT JOIN jid j ON j._id=c.jid_row_id
		WHERE (c._id IS NULL OR (COALESCE(c.hidden,0)=0 AND j._id IS NULL))
		AND m.message_type IS NOT NULL GROUP BY m.message_type`)
	if err != nil {
		return nil, fmt.Errorf("account for excluded orphan types: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[int]int64)
	for rows.Next() {
		var kind int
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		if isSkippedType(kind) {
			counts[kind] = count
		}
	}
	return counts, rows.Err()
}

// fetchMessages returns messages for a chat, batched after a given _id.
// Messages are ordered by _id ascending for deterministic resumability.
// Joins with jid to resolve sender information.
func fetchMessages(ctx context.Context, db *sql.DB, chatRowID int64, afterID int64, limit int) ([]waMessage, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			m._id,
			m.chat_row_id,
			COALESCE(m.from_me, 0),
			COALESCE(m.key_id, ''),
			m.sender_jid_row_id,
			sj.raw_string,
			sj.user,
			sj.server,
			COALESCE(m.timestamp, 0),
			COALESCE(m.message_type, 0),
			m.text_data,
			COALESCE(m.status, 0),
			COALESCE(m.starred, 0)
		FROM message m
		LEFT JOIN jid sj ON m.sender_jid_row_id = sj._id
		WHERE m.chat_row_id = ?
		  AND m._id > ?
		ORDER BY m._id ASC
		LIMIT ?
	`, chatRowID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []waMessage
	for rows.Next() {
		var m waMessage
		if err := rows.Scan(
			&m.RowID, &m.ChatRowID, &m.FromMe, &m.KeyID,
			&m.SenderJIDRowID, &m.SenderRawString, &m.SenderUser, &m.SenderServer,
			&m.Timestamp, &m.MessageType, &m.TextData,
			&m.Status, &m.Starred,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// fetchMedia returns media metadata for a batch of message row IDs.
// Returns a map of message_row_id → waMedia.
// Handles old WhatsApp schemas that lack the media_caption column.
func fetchMedia(ctx context.Context, db *sql.DB, messageRowIDs []int64) (map[int64]waMedia, error) {
	if len(messageRowIDs) == 0 {
		return make(map[int64]waMedia), nil
	}

	hasCaption := hasColumn(ctx, db, "message_media", "media_caption")

	result := make(map[int64]waMedia)

	// Process in chunks to stay within SQLite's parameter limit.
	const chunkSize = 500
	for i := 0; i < len(messageRowIDs); i += chunkSize {
		end := min(i+chunkSize, len(messageRowIDs))
		chunk := messageRowIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		captionExpr := "NULL"
		if hasCaption {
			captionExpr = "mm.media_caption"
		}

		query := fmt.Sprintf(`
			SELECT
				mm.message_row_id,
				mm.mime_type,
				%s,
				mm.file_size,
				mm.file_path,
				mm.width,
				mm.height,
				mm.media_duration
			FROM message_media mm
			WHERE mm.message_row_id IN (%s)
		`, captionExpr, strings.Join(placeholders, ","))

		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("fetch media: %w", err)
		}
		if err := scanMediaChunk(rows, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// scanMediaChunk scans one media query's rows into result and closes the rows.
// Taking ownership of a single *sql.Rows here keeps the deferred Close scoped
// to one query so it cannot leak across the caller's chunk loop.
func scanMediaChunk(rows *sql.Rows, result map[int64]waMedia) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var m waMedia
		if err := rows.Scan(
			&m.MessageRowID, &m.MimeType, &m.MediaCaption,
			&m.FileSize, &m.FilePath, &m.Width, &m.Height,
			&m.MediaDuration,
		); err != nil {
			return fmt.Errorf("scan media: %w", err)
		}
		result[m.MessageRowID] = m
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate media: %w", err)
	}
	return nil
}

// fetchReactions returns reactions for a batch of message row IDs.
// Returns a map of message_row_id → []waReaction.
func fetchReactions(ctx context.Context, db *sql.DB, messageRowIDs []int64) (map[int64][]waReaction, error) {
	if len(messageRowIDs) == 0 {
		return make(map[int64][]waReaction), nil
	}

	result := make(map[int64][]waReaction)
	fromMeExpr := "NULL"
	if hasColumn(ctx, db, "message_add_on", "from_me") {
		fromMeExpr = "ao.from_me"
	}

	const chunkSize = 500
	for i := 0; i < len(messageRowIDs); i += chunkSize {
		end := min(i+chunkSize, len(messageRowIDs))
		chunk := messageRowIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		// WhatsApp stores reactions in message_add_on (metadata) joined with
		// message_add_on_reaction (the actual emoji). The link to the original
		// message is via parent_message_row_id.
		query := fmt.Sprintf(`
			SELECT
				ao.parent_message_row_id,
				ao.sender_jid_row_id,
				%s,
				sj.raw_string,
				sj.user,
				sj.server,
				ar.reaction,
				COALESCE(ar.sender_timestamp, 0)
			FROM message_add_on ao
			JOIN message_add_on_reaction ar ON ar.message_add_on_row_id = ao._id
			LEFT JOIN jid sj ON ao.sender_jid_row_id = sj._id
			WHERE ao.parent_message_row_id IN (%s)
			  AND ar.reaction IS NOT NULL
		`, fromMeExpr, strings.Join(placeholders, ","))

		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			// Table might not exist in older DB versions
			if isTableNotFound(err) {
				return result, nil
			}
			return nil, fmt.Errorf("fetch reactions: %w", err)
		}

		if err := scanReactionChunk(rows, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// scanReactionChunk scans one reaction query's rows into result and closes the
// rows. Owning a single *sql.Rows here scopes the deferred Close to one query
// so it cannot leak across the caller's chunk loop.
func scanReactionChunk(rows *sql.Rows, result map[int64][]waReaction) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var r waReaction
		if err := rows.Scan(
			&r.MessageRowID, &r.SenderJIDRowID, &r.FromMe,
			&r.SenderRawString, &r.SenderUser, &r.SenderServer,
			&r.ReactionValue, &r.Timestamp,
		); err != nil {
			return fmt.Errorf("scan reaction: %w", err)
		}
		result[r.MessageRowID] = append(result[r.MessageRowID], r)
	}
	return rows.Err()
}

// groupRosterSchema recognizes supported layouts instead of treating an
// unreadable roster as a complete empty group.
func groupRosterSchema(ctx context.Context, db *sql.DB) string {
	if hasColumn(ctx, db, "group_participant_user", "group_jid_row_id") &&
		hasColumn(ctx, db, "group_participant_user", "user_jid_row_id") &&
		hasColumn(ctx, db, "group_participant_user", "rank") &&
		hasColumn(ctx, db, "group_participant_user", "pending") {
		return "modern"
	}
	if hasColumn(ctx, db, "group_participants", "gjid") &&
		hasColumn(ctx, db, "group_participants", "jid") &&
		hasColumn(ctx, db, "group_participants", "admin") {
		return "legacy"
	}
	return "unavailable"
}

// countOrphanRosterGroups catches entries whose group cannot be reached by a
// per-chat lookup. A hidden chat is present even though it is not imported.
func countOrphanRosterGroups(ctx context.Context, db *sql.DB) (int64, error) {
	var query string
	switch groupRosterSchema(ctx, db) {
	case "modern":
		query = `SELECT COUNT(*) FROM group_participant_user p LEFT JOIN jid g ON g._id = p.group_jid_row_id WHERE g._id IS NULL OR NOT EXISTS (SELECT 1 FROM chat c WHERE c.jid_row_id = g._id)`
	case "legacy":
		query = `SELECT COUNT(*) FROM group_participants p LEFT JOIN jid g ON g.raw_string = p.gjid WHERE g._id IS NULL OR NOT EXISTS (SELECT 1 FROM chat c WHERE c.jid_row_id = g._id)`
	default:
		return 0, nil
	}
	var count int64
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("check group roster references: %w", err)
	}
	return count, nil
}

// fetchGroupParticipants reads legacy or modern rosters. Modern LIDs resolve
// only through jid_map; they must never be interpreted as phone numbers.
// Modern rank (1=admin, 2=superadmin) and pending=1 as unaccepted membership
// are corroborated by the independent WAInsight importer, not an official
// WhatsApp specification:
// https://github.com/akhil-dara/WAInsight/blob/main/backend/app/ingestion/chat_ingester.py
func fetchGroupParticipants(ctx context.Context, db *sql.DB, groupJIDRawString string) ([]waGroupMember, error) {
	var query string
	switch groupRosterSchema(ctx, db) {
	case "modern":
		if !hasColumn(ctx, db, "jid_map", "lid_row_id") || !hasColumn(ctx, db, "jid_map", "jid_row_id") {
			return nil, errors.New("modern group roster cannot resolve LIDs: jid_map unavailable")
		}
		query = `SELECT g.raw_string, COALESCE(u.raw_string, ''),
			COALESCE(phone.user, u.user, ''), COALESCE(phone.server, u.server, ''),
			COALESCE(p.rank, -1), COALESCE(p.pending, -1)
			FROM group_participant_user p
			JOIN jid g ON g._id = p.group_jid_row_id
			LEFT JOIN jid u ON u._id = p.user_jid_row_id
			LEFT JOIN jid_map jm ON jm.lid_row_id = u._id
			LEFT JOIN jid phone ON phone._id = jm.jid_row_id
			WHERE g.raw_string = ?`
	case "legacy":
		query = `SELECT gp.gjid, gp.jid, COALESCE(j.user, ''), COALESCE(j.server, ''),
			COALESCE(gp.admin, -1), 0
			FROM group_participants gp LEFT JOIN jid j ON gp.jid = j.raw_string
			WHERE gp.gjid = ?`
	default:
		return nil, errors.New("group roster schema unavailable")
	}
	rows, err := db.QueryContext(ctx, query, groupJIDRawString)
	if err != nil {
		return nil, fmt.Errorf("fetch group participants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []waGroupMember
	for rows.Next() {
		var m waGroupMember
		if err := rows.Scan(&m.GroupJID, &m.MemberJID, &m.MemberUser, &m.MemberServer, &m.Admin, &m.Pending); err != nil {
			return nil, fmt.Errorf("scan group participant: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// fetchQuotedMessages returns quoted message references for a batch of message row IDs.
// Returns a map of message_row_id → waQuoted (the message that contains the quote).
func fetchQuotedMessages(ctx context.Context, db *sql.DB, messageRowIDs []int64) (map[int64]waQuoted, error) {
	if len(messageRowIDs) == 0 {
		return make(map[int64]waQuoted), nil
	}

	result := make(map[int64]waQuoted)

	const chunkSize = 500
	for i := 0; i < len(messageRowIDs); i += chunkSize {
		end := min(i+chunkSize, len(messageRowIDs))
		chunk := messageRowIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		query := fmt.Sprintf(`
			SELECT
				mq.message_row_id,
				mq.key_id
			FROM message_quoted mq
			WHERE mq.message_row_id IN (%s)
			  AND mq.key_id IS NOT NULL
			  AND mq.key_id != ''
		`, strings.Join(placeholders, ","))

		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			if isTableNotFound(err) {
				return result, nil
			}
			return nil, fmt.Errorf("fetch quoted messages: %w", err)
		}

		if err := scanQuotedChunk(rows, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// scanQuotedChunk scans one quoted-message query's rows into result and closes
// the rows. Owning a single *sql.Rows here scopes the deferred Close to one
// query so it cannot leak across the caller's chunk loop.
func scanQuotedChunk(rows *sql.Rows, result map[int64]waQuoted) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var q waQuoted
		if err := rows.Scan(&q.MessageRowID, &q.QuotedKeyID); err != nil {
			return fmt.Errorf("scan quoted message: %w", err)
		}
		result[q.MessageRowID] = q
	}
	return rows.Err()
}

// fetchLidMap reads the WhatsApp jid_map table to build a mapping from
// lid JID row IDs to their corresponding phone JIDs. The jid_map table
// links two jid rows: one with server="lid" and one with the real phone
// (server="s.whatsapp.net"). Returns an empty map gracefully if the
// jid_map table doesn't exist (older WhatsApp DB versions).
func fetchLidMap(ctx context.Context, db *sql.DB) (map[int64]waLidMapping, error) {
	result := make(map[int64]waLidMapping)

	rows, err := db.QueryContext(ctx, `
		SELECT
			jm.lid_row_id,
			COALESCE(phone_jid.user, ''),
			COALESCE(phone_jid.server, '')
		FROM jid_map jm
		JOIN jid phone_jid ON jm.jid_row_id = phone_jid._id
	`)
	if err != nil {
		if isTableNotFound(err) {
			return result, nil
		}
		return nil, fmt.Errorf("fetch lid map: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var m waLidMapping
		if err := rows.Scan(&m.LidRowID, &m.PhoneUser, &m.PhoneServer); err != nil {
			return nil, fmt.Errorf("scan lid mapping: %w", err)
		}
		if previous, ok := result[m.LidRowID]; ok && (previous.PhoneUser != m.PhoneUser || previous.PhoneServer != m.PhoneServer) {
			return nil, errors.New("WhatsApp LID has conflicting authoritative phone mappings")
		}
		result[m.LidRowID] = m
	}
	return result, rows.Err()
}

// isTableNotFound returns true if the error indicates a missing table.
func isTableNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
