package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
)

type whatsAppSourceRecord struct {
	Kind            string `json:"kind"`
	MessageType     int    `json:"message_type"`
	TimestampMillis int64  `json:"timestamp_millis"`
}

// ObserveWhatsAppSystemEventContext annotates a legacy archived row only when
// a source observation identifies the same message, conversation, and date as
// a supported system event. Type 112 is an advanced-chat-privacy setting; other
// types need their own reviewed classification. Missing rows are not inserted.
// The annotation is reversible through SetMessageMetadataContext; no content,
// sender, or message identity is replaced.
func (s *Store) ObserveWhatsAppSystemEventContext(
	ctx context.Context, sourceID, conversationID int64, key string, sourceType int, timestampMillis int64,
) (bool, error) {
	if sourceType != 112 || key == "" || timestampMillis <= 0 {
		return false, errors.New("WhatsApp system event requires a type-112 source key and timestamp")
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return false, err
	}
	changed := false
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		var sourceKind string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id=?`, sourceID).Scan(&sourceKind); err != nil {
			return fmt.Errorf("read WhatsApp system event source: %w", err)
		}
		if sourceKind != whatsAppService {
			return errors.New("WhatsApp system event requires a WhatsApp source")
		}
		var messageID, archivedConversation int64
		var messageKind string
		var sent sql.NullTime
		var metadata sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id,conversation_id,message_type,sent_at,metadata
			FROM messages WHERE source_id=? AND source_message_id=?`, sourceID, key).
			Scan(&messageID, &archivedConversation, &messageKind, &sent, &metadata)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read legacy WhatsApp system event: %w", err)
		}
		if messageKind != whatsAppService || archivedConversation != conversationID || !sent.Valid || !sent.Time.Equal(time.UnixMilli(timestampMillis)) {
			return errors.New("WhatsApp system event conflicts with archived message identity or date")
		}
		observed := whatsAppSourceRecord{Kind: "system_event", MessageType: sourceType, TimestampMillis: timestampMillis}
		updated, annotationChanged, err := whatsAppSystemEventMetadata(metadata, observed)
		if err != nil || !annotationChanged {
			return err
		}
		if err := setMessageMetadataWith(boundQuerier{ctx: ctx, q: tx}, s.dialect, messageID, updated); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func whatsAppSystemEventMetadata(metadata sql.NullString, observed whatsAppSourceRecord) (sql.NullString, bool, error) {
	fields := make(map[string]jsontext.Value)
	if metadata.Valid {
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return sql.NullString{}, false, fmt.Errorf("read WhatsApp system event metadata: %w", err)
		}
	}
	if fields == nil {
		fields = make(map[string]jsontext.Value)
	}
	if raw, exists := fields["whatsapp_source_record"]; exists {
		var previous whatsAppSourceRecord
		if err := json.Unmarshal(raw, &previous); err != nil {
			return sql.NullString{}, false, fmt.Errorf("read previous WhatsApp source classification: %w", err)
		}
		if previous != observed {
			return sql.NullString{}, false, errors.New("WhatsApp system event conflicts with retained source classification")
		}
		return metadata, false, nil
	}
	encodedObservation, err := json.Marshal(observed)
	if err != nil {
		return sql.NullString{}, false, err
	}
	fields["whatsapp_source_record"] = encodedObservation
	encoded, err := json.Marshal(fields)
	if err != nil {
		return sql.NullString{}, false, err
	}
	return sql.NullString{String: string(encoded), Valid: true}, true, nil
}
