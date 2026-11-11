package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
)

type retainedSystemEvents struct {
	classified  int64
	blockedKeys map[string]struct{}
}

// classifyRetainedSystemEvents repairs history no longer present in the phone
// snapshot, using only the original Android payload retained with each row.
// An empty message alone never establishes a system event or a missing author.
func (imp *Importer) classifyRetainedSystemEvents(ctx context.Context, waDB *sql.DB, sourceID int64, onError func(error)) (retainedSystemEvents, error) {
	result := retainedSystemEvents{blockedKeys: make(map[string]struct{})}
	reject := func(key string, messageID int64, err error) {
		result.blockedKeys[key] = struct{}{}
		onError(fmt.Errorf("retained WhatsApp message %d: %w", messageID, err))
	}
	var afterID int64
	for {
		candidates, err := imp.store.ScanWhatsAppSystemEventCandidatesContext(ctx, sourceID, afterID, 100)
		if err != nil {
			return result, err
		}
		if len(candidates) == 0 {
			return result, nil
		}
		for _, candidate := range candidates {
			afterID = candidate.MessageID
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if candidate.RawDecodeError != nil {
				reject(candidate.Key, candidate.MessageID, candidate.RawDecodeError)
				continue
			}
			var observed waMessage
			if err := json.Unmarshal(candidate.RawData, &observed); err != nil {
				reject(candidate.Key, candidate.MessageID, fmt.Errorf("parse retained source: %w", err))
				continue
			}
			if err := checkRetainedSystemEventSnapshot(ctx, waDB, candidate.Key, observed); err != nil {
				reject(candidate.Key, candidate.MessageID, err)
				continue
			}
			if observed.MessageType != 112 {
				continue
			}
			if observed.KeyID == "" || observed.KeyID != candidate.Key || observed.Timestamp <= 0 ||
				!candidate.SentAt.Valid || !candidate.SentAt.Time.Equal(time.UnixMilli(observed.Timestamp)) {
				reject(candidate.Key, candidate.MessageID, errors.New("source system event conflicts with archived identity or date"))
				continue
			}
			changed, err := imp.store.ObserveWhatsAppSystemEventContext(ctx, sourceID, candidate.ConversationID,
				observed.KeyID, observed.MessageType, observed.Timestamp)
			if err != nil {
				reject(candidate.Key, candidate.MessageID, err)
			} else if changed {
				result.classified++
			}
		}
	}
}

// Neither source is silently discarded when a retained or current observation
// identifies a system event. Leave the exact key untouched for reconciliation.
func checkRetainedSystemEventSnapshot(ctx context.Context, waDB *sql.DB, key string, retained waMessage) error {
	rows, err := waDB.QueryContext(ctx, `SELECT message_type,timestamp FROM message WHERE key_id=? LIMIT 2`, key)
	if err != nil {
		return fmt.Errorf("read current system event evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var count int
	for rows.Next() {
		count++
		var kind, timestamp sql.NullInt64
		if err := rows.Scan(&kind, &timestamp); err != nil {
			return fmt.Errorf("read current system event evidence: %w", err)
		}
		if retained.MessageType == 112 || kind.Valid && kind.Int64 == 112 {
			if count > 1 || !kind.Valid || kind.Int64 != int64(retained.MessageType) ||
				!timestamp.Valid || timestamp.Int64 != retained.Timestamp {
				return errors.New("retained and current system event observations conflict")
			}
		}
	}
	return rows.Err()
}
