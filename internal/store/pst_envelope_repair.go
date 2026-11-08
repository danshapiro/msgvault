package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/mail"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/textutil"
)

// applyPSTRepairEnvelope fills absent facts, or replaces only values proven
// equal to the original parser's output. Existing recipient snapshots and
// links remain authoritative. Caller holds source, message and raw locks.
func (s *Store) applyPSTRepairEnvelope(ctx context.Context, tx *loggedTx, sourceID, messageID int64, env *PSTRepairEnvelope) (bool, []string, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	var subject, rfcID, metadata sql.NullString
	var sent sql.NullTime
	var sender, reply sql.NullInt64
	err := q.QueryRow(`SELECT subject, sent_at, sender_id, rfc822_message_id, metadata, reply_to_message_id
		FROM messages WHERE id = ? AND source_id = ?`, messageID, sourceID).Scan(&subject, &sent, &sender, &rfcID, &metadata, &reply)
	if err != nil {
		return false, nil, fmt.Errorf("read PST repair envelope: %w", err)
	}
	var changed bool
	var preserved []string
	newSubject := textutil.EnsureUTF8(env.Subject)
	oldSubject := textutil.EnsureUTF8(env.ExpectedSubject)
	var subjectChanged bool
	if newSubject != "" && subject.String != newSubject {
		if subject.String == "" || subject.String == oldSubject {
			if _, err := q.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, newSubject, messageID); err != nil {
				return false, nil, fmt.Errorf("repair PST subject: %w", err)
			}
			subjectChanged = true
			changed = true
		} else {
			preserved = append(preserved, "subject")
		}
	}
	if subjectChanged {
		var conversationTitle sql.NullString
		if err := q.QueryRow(`SELECT c.title FROM conversations c JOIN messages m ON m.conversation_id = c.id WHERE m.id = ?`, messageID).Scan(&conversationTitle); err != nil {
			return false, nil, fmt.Errorf("read PST conversation title: %w", err)
		}
		if conversationTitle.String != newSubject {
			preserved = append(preserved, "conversation subject not changed")
		}
	}
	if !env.SentAt.IsZero() && (!sent.Valid || !sent.Time.Equal(env.SentAt)) {
		if !sent.Valid || !env.ExpectedSentAt.IsZero() && sent.Time.Equal(env.ExpectedSentAt) {
			if _, err := q.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, env.SentAt.UTC(), messageID); err != nil {
				return false, nil, fmt.Errorf("repair PST date: %w", err)
			}
			if _, err := q.Exec(`UPDATE conversations SET last_message_at = (
				SELECT MAX(COALESCE(sent_at, received_at, internal_date)) FROM messages
				WHERE conversation_id = conversations.id AND deleted_at IS NULL
			) WHERE id = (SELECT conversation_id FROM messages WHERE id = ?)`, messageID); err != nil {
				return false, nil, fmt.Errorf("refresh PST conversation date: %w", err)
			}
			changed = true
		} else {
			preserved = append(preserved, "date")
		}
	}
	addressChanged, addressPreserved, err := s.repairPSTRecipientRoles(ctx, tx, messageID, sender, env)
	if err != nil {
		return false, nil, err
	}
	changed = changed || addressChanged
	preserved = append(preserved, addressPreserved...)
	newID := mime.NormalizeMessageID(env.MessageID)
	parent := mime.NormalizeMessageID(env.InReplyTo)
	thread := mime.NormalizeMessageID(env.ThreadKey)
	acceptedID := mime.NormalizeMessageID(rfcID.String)
	if rfcID.String != "" && newID != "" && acceptedID != newID {
		preserved = append(preserved, "message-id")
		// Do not derive reply links from any metadata when the source ID
		// conflicts with the existing message identity.
		return changed, preserved, nil
	} else if rfcID.String == "" && newID != "" {
		if _, err := q.Exec(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`, newID, messageID); err != nil {
			return false, nil, fmt.Errorf("repair PST message-id: %w", err)
		}
		changed = true
		// A focused one-message repair does not scan or merge other
		// conversations. Existing unlinked children can be reconciled later.
		preserved = append(preserved, "child reply links not reconciled")
	}
	if reply.Valid && parent != "" {
		var linkedID sql.NullString
		err := q.QueryRow(`SELECT rfc822_message_id FROM messages WHERE id = ?`, reply.Int64).Scan(&linkedID)
		if err != nil {
			return false, nil, fmt.Errorf("read existing PST reply link: %w", err)
		}
		if mime.NormalizeMessageID(linkedID.String) != parent {
			// A nonempty existing link is authoritative; do not add source
			// metadata that would describe a different reply chain.
			preserved = append(preserved, "reply link")
			return changed, preserved, nil
		}
	}
	if parent != "" || thread != "" {
		if metadata.Valid && metadata.String != "" {
			var existing struct {
				Parent string `json:"email_in_reply_to"`
				Thread string `json:"pst_thread_key"`
			}
			if err := json.Unmarshal([]byte(metadata.String), &existing); err != nil {
				return false, nil, fmt.Errorf("decode PST thread metadata: %w", err)
			}
			if parent != "" && existing.Parent != "" && mime.NormalizeMessageID(existing.Parent) != parent {
				preserved = append(preserved, "reply metadata")
			}
			if thread != "" && existing.Thread != "" && mime.NormalizeMessageID(existing.Thread) != thread {
				preserved = append(preserved, "thread metadata")
			}
		}
		merged, updated, err := mergeEmailHeaderMetadata(metadata, parent, thread)
		if err != nil {
			return false, nil, fmt.Errorf("repair PST thread metadata: %w", err)
		}
		if updated {
			if _, err := q.Exec(fmt.Sprintf(`UPDATE messages SET metadata = %s WHERE id = ?`, s.dialect.JSONBindExpr()), merged, messageID); err != nil {
				return false, nil, fmt.Errorf("repair PST thread metadata: %w", err)
			}
			metadata = sql.NullString{String: merged, Valid: true}
			changed = true
		}
	}
	// Resolve only the target's direct parent. Never replace links or merge
	// existing conversations.
	if metadata.Valid {
		var facts struct {
			Parent string `json:"email_in_reply_to"`
		}
		if err := json.Unmarshal([]byte(metadata.String), &facts); err != nil {
			return false, nil, fmt.Errorf("decode PST reply metadata: %w", err)
		}
		storedParent := mime.NormalizeMessageID(facts.Parent)
		if parent != "" && storedParent != parent {
			return changed, preserved, nil
		}
		if storedParent != "" && s.IsPostgreSQL() {
			if !reply.Valid {
				preserved = append(preserved, "reply link not resolved on PostgreSQL")
			}
			return changed, preserved, nil
		}
		parentID, unique, err := s.uniquePSTMessageByRFCID(ctx, tx, sourceID, storedParent)
		if err != nil {
			return false, nil, err
		}
		if storedParent != "" && (!unique || parentID == messageID) {
			preserved = append(preserved, "reply parent missing or ambiguous")
		} else if storedParent != "" && reply.Valid && reply.Int64 != parentID {
			preserved = append(preserved, "reply link")
		} else if storedParent != "" && !reply.Valid {
			if _, err := q.Exec(`UPDATE messages SET reply_to_message_id = ? WHERE id = ? AND reply_to_message_id IS NULL`, parentID, messageID); err != nil {
				return false, nil, fmt.Errorf("repair PST direct reply: %w", err)
			}
			changed = true
		}
	}
	return changed, preserved, nil
}

func (s *Store) uniquePSTMessageByRFCID(ctx context.Context, tx *loggedTx, sourceID int64, id string) (int64, bool, error) {
	if id == "" {
		return 0, false, nil
	}
	var canonical any = id
	if !s.IsPostgreSQL() {
		canonical = []byte(id)
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM messages WHERE %s = ? AND source_id = ? AND message_type = 'email' AND deleted_at IS NULL ORDER BY id LIMIT 2`, s.dialect.RFC822CanonicalIDExpr("rfc822_message_id")), canonical, sourceID)
	if err != nil {
		return 0, false, fmt.Errorf("find PST reply parent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var found int64
	count := 0
	for rows.Next() {
		if err := rows.Scan(&found); err != nil {
			return 0, false, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return found, count == 1, nil
}

func (s *Store) repairPSTRecipientRoles(ctx context.Context, tx *loggedTx, messageID int64, sender sql.NullInt64, env *PSTRepairEnvelope) (bool, []string, error) {
	const fromRole = "from"
	q := boundQuerier{ctx: ctx, q: tx}
	type rolePlan struct {
		role      string
		addresses []mime.Address
	}
	var plans []rolePlan
	var preserved []string
	var senderFilled bool
	var plannedSMTPFrom bool
	var existingSenderEmail string
	for _, role := range []rolePlan{{fromRole, env.From}, {"to", env.To}, {"cc", env.Cc}, {"bcc", env.Bcc}} {
		if len(role.addresses) == 0 {
			continue
		}
		valid := make([]mime.Address, 0, len(role.addresses))
		invalid := false
		for _, addr := range role.addresses {
			email := strings.ToLower(strings.TrimSpace(textutil.SanitizeUTF8(addr.Email)))
			parsed, err := mail.ParseAddress(email)
			if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, email) || !strings.Contains(email, "@") {
				invalid = true
				continue
			}
			addr.Email, addr.Name = email, textutil.SanitizeUTF8(addr.Name)
			_, addr.Domain, _ = strings.CutLast(email, "@")
			valid = append(valid, addr)
		}
		if invalid {
			preserved = append(preserved, role.role+"-unresolved")
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT r.participant_id, COALESCE(NULLIF(r.email_address, ''), p.email_address, '')
			FROM message_recipients r JOIN participants p ON p.id = r.participant_id
			WHERE r.message_id = ? AND LOWER(r.recipient_type) = ? ORDER BY r.id`, messageID, role.role)
		if err != nil {
			return false, nil, err
		}
		var existingEmails []string
		var matchingSenderID int64
		for rows.Next() {
			var id int64
			var email string
			if err := rows.Scan(&id, &email); err != nil {
				_ = rows.Close()
				return false, nil, err
			}
			email = strings.ToLower(strings.TrimSpace(email))
			if len(valid) > 0 && email == valid[0].Email {
				matchingSenderID = id
			}
			existingEmails = append(existingEmails, email)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, nil, err
		}
		if err := rows.Close(); err != nil {
			return false, nil, err
		}
		if len(existingEmails) > 0 {
			wanted := make(map[string]bool, len(valid))
			for _, addr := range valid {
				wanted[addr.Email] = true
			}
			actual := make(map[string]bool, len(existingEmails))
			for _, email := range existingEmails {
				actual[email] = true
			}
			match := len(wanted) == len(actual)
			for email := range wanted {
				if !actual[email] {
					match = false
				}
			}
			if role.role == fromRole && sender.Valid && sender.Int64 != matchingSenderID {
				match = false
			}
			if !match {
				preserved = append(preserved, role.role)
				continue
			}
			if role.role == fromRole && !sender.Valid && matchingSenderID != 0 {
				if _, err := q.Exec(`UPDATE messages SET sender_id = ? WHERE id = ? AND sender_id IS NULL`, matchingSenderID, messageID); err != nil {
					return false, nil, err
				}
				senderFilled = true
			}
			continue
		}
		if role.role == fromRole && sender.Valid {
			var senderEmail sql.NullString
			if err := q.QueryRow(`SELECT email_address FROM participants WHERE id = ?`, sender.Int64).Scan(&senderEmail); err != nil {
				return false, nil, fmt.Errorf("read existing PST sender: %w", err)
			}
			if !senderEmail.Valid || len(valid) == 0 || !strings.EqualFold(strings.TrimSpace(senderEmail.String), valid[0].Email) {
				preserved = append(preserved, role.role)
				continue
			}
			existingSenderEmail = valid[0].Email
		}
		if len(valid) > 0 {
			plans = append(plans, rolePlan{role.role, valid})
			if role.role == fromRole {
				plannedSMTPFrom = true
			}
		}
	}
	if !senderFilled && !plannedSMTPFrom && env.UnresolvedSender {
		var count int
		if err := q.QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND LOWER(recipient_type) = 'from'`, messageID).Scan(&count); err != nil {
			return false, nil, err
		}
		if !sender.Valid || count == 0 {
			preserved = append(preserved, "unresolved sender")
		}
	}
	if !sender.Valid && !senderFilled && !plannedSMTPFrom && !env.UnresolvedSender {
		preserved = append(preserved, "sender unavailable")
	}
	if sender.Valid && len(env.From) == 0 {
		var count int
		if err := q.QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND LOWER(recipient_type) = 'from'`, messageID).Scan(&count); err != nil {
			return false, nil, err
		}
		if count == 0 {
			preserved = append(preserved, "from unavailable")
		}
	}
	for _, role := range env.UnresolvedRecipients {
		role = strings.ToLower(strings.TrimSpace(role))
		var diagnostic string
		switch role {
		case "to", "cc", "bcc":
			diagnostic = role + "-unresolved"
		case "to-unresolved", "cc-unresolved", "bcc-unresolved", "recipient-unresolved", "recipient metadata unreadable":
			diagnostic = role
		default:
			diagnostic = "recipient-unresolved"
		}
		if !slices.Contains(preserved, diagnostic) {
			preserved = append(preserved, diagnostic)
		}
	}
	if len(plans) == 0 && !senderFilled {
		return false, preserved, nil
	}
	// Deterministic insertion order avoids participant lock inversions.
	all := make(map[string]mime.Address)
	for _, plan := range plans {
		for _, a := range plan.addresses {
			if _, ok := all[a.Email]; !ok {
				all[a.Email] = a
			}
		}
	}
	emails := make([]string, 0, len(all))
	for email := range all {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	ids := make(map[string]int64, len(emails))
	inserted := false
	for _, email := range emails {
		if existingSenderEmail != "" && email == existingSenderEmail {
			ids[email] = sender.Int64
			continue
		}
		a := all[email]
		id, err := ensureParticipantWith(q, s.dialect, email, a.Name, a.Domain, func() error { inserted = true; return nil })
		if err != nil {
			return false, nil, fmt.Errorf("ensure PST participant: %w", err)
		}
		ids[email] = id
	}
	if inserted {
		if err := s.bumpParticipantDisplayNameRevisionContext(ctx, tx); err != nil {
			return false, nil, err
		}
	}
	for _, plan := range plans {
		set := RecipientSet{Type: plan.role}
		for _, a := range plan.addresses {
			set.ParticipantIDs = append(set.ParticipantIDs, ids[a.Email])
			set.DisplayNames = append(set.DisplayNames, a.Name)
			set.EmailAddresses = append(set.EmailAddresses, a.Email)
		}
		if err := replaceMessageRecipientsTx(q, messageID, set); err != nil {
			return false, nil, fmt.Errorf("repair PST %s: %w", plan.role, err)
		}
		if plan.role == fromRole {
			if _, err := q.Exec(`UPDATE messages SET sender_id = ? WHERE id = ? AND sender_id IS NULL`, set.ParticipantIDs[0], messageID); err != nil {
				return false, nil, err
			}
		}
	}
	if err := refreshMessageAttributionWith(q, messageID); err != nil {
		return false, nil, err
	}
	if _, err := s.refreshAccountAttributionTx(ctx, tx, messageID, deliveryInput{}); err != nil {
		return false, nil, err
	}
	return len(plans) > 0 || senderFilled, preserved, nil
}
