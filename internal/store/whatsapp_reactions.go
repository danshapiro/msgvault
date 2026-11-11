package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

type whatsAppReactionState struct {
	Timestamp int64  `json:"timestamp"`
	Value     string `json:"value"`
}

// ObserveWhatsAppReactionContext applies an explicit actor observation. Empty
// values retain removal timestamps in provider metadata, so an older backup
// cannot resurrect the reaction. Absent source rows never imply removal.
// Durable identity aliases keep this ordering intact after a LID is resolved.
func (s *Store) ObserveWhatsAppReactionContext(ctx context.Context, messageID, participantID int64, value string, timestamp int64) error {
	return s.withMessageAttributionTxContext(ctx, messageID, func(tx *loggedTx) error {
		return s.observeWhatsAppReactionTx(ctx, tx, messageID, participantID, value, timestamp)
	})
}

func (s *Store) observeWhatsAppReactionTx(ctx context.Context, tx *loggedTx, messageID, participantID int64, value string, timestamp int64) error {
	q := boundQuerier{ctx: ctx, q: tx}
	if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
		return err
	}
	var metadata sql.NullString
	var sourceType, account string
	if err := tx.QueryRowContext(ctx, `SELECT m.metadata,s.source_type,s.identifier
			FROM messages m JOIN sources s ON s.id=m.source_id WHERE m.id=?`, messageID).Scan(&metadata, &sourceType, &account); err != nil {
		return err
	}
	if sourceType != "whatsapp" {
		return errors.New("WhatsApp reaction requires a WhatsApp message")
	}
	aliases, err := whatsAppReactionAliasesTx(ctx, tx, participantID, account)
	if err != nil {
		return err
	}
	fields := make(map[string]jsontext.Value)
	if metadata.Valid {
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return fmt.Errorf("read WhatsApp reaction metadata: %w", err)
		}
	}
	if fields == nil {
		fields = make(map[string]jsontext.Value)
	}
	states := make(map[string]whatsAppReactionState)
	if raw := fields["whatsapp_reactions"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &states); err != nil {
			return fmt.Errorf("read WhatsApp reaction ordering: %w", err)
		}
	}
	if states == nil {
		states = make(map[string]whatsAppReactionState)
	}
	for _, alias := range aliases {
		if previous, found := states[alias]; found {
			if previous.Timestamp > timestamp {
				return nil
			}
			if previous.Timestamp == timestamp && previous.Value != value {
				return errors.New("WhatsApp reaction conflicts with retained observation at the same timestamp")
			}
		}
	}
	// Bootstrap ordering from reactions imported before provider metadata
	// existed. Unknown timestamps are weaker than an explicit observation.
	visible, err := whatsAppVisibleReactionsTx(ctx, tx, messageID, participantID)
	if err != nil {
		return err
	}
	for _, previous := range visible {
		if previous.Timestamp > timestamp {
			return nil
		}
	}
	for _, previous := range visible {
		if previous.Timestamp == timestamp && previous.Value != value {
			return errors.New("WhatsApp reaction conflicts with archived reaction at the same timestamp")
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE message_id=? AND participant_id=? AND reaction_type='emoji'`, messageID, participantID); err != nil {
		return err
	}
	if value != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reactions (message_id,participant_id,reaction_type,reaction_value,created_at)
				VALUES (?,?,'emoji',?,?)`, messageID, participantID, value, time.UnixMilli(timestamp).UTC()); err != nil {
			return err
		}
	}
	for _, alias := range aliases {
		delete(states, alias)
	}
	states[aliases[0]] = whatsAppReactionState{Timestamp: timestamp, Value: value}
	fields["whatsapp_reactions"], err = json.Marshal(states)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return setMessageMetadataWith(q, s.dialect, messageID, sql.NullString{String: string(encoded), Valid: true})
}

func whatsAppReactionAliasesTx(ctx context.Context, tx *loggedTx, participantID int64, account string) ([]string, error) {
	var phone string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(phone_number,'') FROM participants WHERE id=?`, participantID).Scan(&phone); err != nil {
		return nil, err
	}
	aliases := []string{}
	if phone != "" {
		aliases = append(aliases, phone)
	}
	rows, err := tx.QueryContext(ctx, `SELECT identifier_value FROM participant_identifiers
			WHERE participant_id=? AND identifier_type='whatsapp' ORDER BY identifier_value`, participantID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if strings.HasPrefix(alias, whatsAppLIDIdentifier(account, "")) {
			aliases = append(aliases, alias)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(aliases) == 0 {
		return nil, errors.New("WhatsApp reactor has no retained phone or source-scoped LID identity")
	}
	return aliases, nil
}

// Reconcile merged actors on historical messages as well as this backup's
// messages. A retained removal can otherwise leave the phone actor's older
// visible emoji behind when the opaque actor is merged into it.
func (s *Store) reconcileWhatsAppMergedReactionsTx(ctx context.Context, tx *loggedTx, sourceID, participantID int64, account string) error {
	aliases, err := whatsAppReactionAliasesTx(ctx, tx, participantID, account)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,metadata FROM messages m WHERE source_id=?
		AND (metadata IS NOT NULL OR EXISTS (SELECT 1 FROM reactions r WHERE r.message_id=m.id AND r.participant_id=?))`, sourceID, participantID)
	if err != nil {
		return err
	}
	type candidate struct {
		id       int64
		metadata sql.NullString
	}
	var messages []candidate
	for rows.Next() {
		var m candidate
		if err := rows.Scan(&m.id, &m.metadata); err != nil {
			_ = rows.Close()
			return err
		}
		messages = append(messages, m)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range messages {
		var latest whatsAppReactionState
		found, conflict := false, false
		consider := func(observation whatsAppReactionState) {
			if !found || observation.Timestamp > latest.Timestamp {
				latest, found, conflict = observation, true, false
			} else if observation.Timestamp == latest.Timestamp && observation.Value != latest.Value {
				conflict = true
			}
		}
		if m.metadata.Valid {
			var fields struct {
				Reactions map[string]whatsAppReactionState `json:"whatsapp_reactions"`
			}
			if err := json.Unmarshal([]byte(m.metadata.String), &fields); err != nil {
				return fmt.Errorf("read merged WhatsApp reaction metadata: %w", err)
			}
			for _, alias := range aliases {
				if observation, ok := fields.Reactions[alias]; ok {
					consider(observation)
				}
			}
		}
		visible, err := whatsAppVisibleReactionsTx(ctx, tx, m.id, participantID)
		if err != nil {
			return err
		}
		for _, observation := range visible {
			consider(observation)
		}
		if conflict {
			return errors.New("WhatsApp LID resolution conflicts with latest historical reaction evidence")
		}
		if found {
			if err := s.observeWhatsAppReactionTx(ctx, tx, m.id, participantID, latest.Value, latest.Timestamp); err != nil {
				return err
			}
		}
	}
	return nil
}

func whatsAppVisibleReactionsTx(ctx context.Context, tx *loggedTx, messageID, participantID int64) ([]whatsAppReactionState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT reaction_value,created_at FROM reactions
		WHERE message_id=? AND participant_id=? AND reaction_type='emoji'`, messageID, participantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var observations []whatsAppReactionState
	for rows.Next() {
		var observation whatsAppReactionState
		var created sql.NullTime
		if err := rows.Scan(&observation.Value, &created); err != nil {
			return nil, err
		}
		if created.Valid {
			observation.Timestamp = created.Time.UnixMilli()
		}
		observations = append(observations, observation)
	}
	return observations, rows.Err()
}
