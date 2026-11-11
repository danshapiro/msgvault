package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const whatsAppLIDPrefix = "whatsapp-lid:"

func whatsAppLIDIdentifier(account, jid string) string {
	return whatsAppLIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(account)) + ":" + jid
}

// EnsureWhatsAppLIDParticipantContext retains an account-scoped provider
// identity even when the backup cannot name its owner. Only an authoritative
// phone mapping may resolve it; conflicting prior identity or membership
// evidence aborts the resolution without repointing anything.
func (s *Store) EnsureWhatsAppLIDParticipantContext(ctx context.Context, sourceID int64, account, jid, phone string) (int64, error) {
	if account == "" || !strings.HasSuffix(jid, "@lid") || strings.TrimSuffix(jid, "@lid") == "" {
		return 0, errors.New("WhatsApp account and full LID are required")
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return 0, err
	}
	var sourceAccount, sourceType string
	if err := s.db.QueryRowContext(ctx, `SELECT identifier,source_type FROM sources WHERE id=?`, sourceID).Scan(&sourceAccount, &sourceType); err != nil {
		return 0, fmt.Errorf("read WhatsApp identity source: %w", err)
	}
	if sourceType != "whatsapp" || sourceAccount != account {
		return 0, errors.New("WhatsApp identity belongs to a different source account")
	}
	value := whatsAppLIDIdentifier(account, jid)
	if phone == "" {
		return s.EnsureParticipantByIdentifierContext(ctx, "whatsapp", value, "")
	}

	var participantID int64
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		if err := s.lockParticipantDirectoryMutationTxContext(ctx, tx); err != nil {
			return err
		}
		previousID, exists, err := participantIdentifierTargetTx(ctx, tx, "whatsapp", value)
		if err != nil {
			return err
		}
		if exists {
			var previousPhone string
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(phone_number,'') FROM participants WHERE id=?`, previousID).Scan(&previousPhone); err != nil {
				return err
			}
			if previousPhone != "" && previousPhone != phone {
				return errors.New("WhatsApp LID mapping conflicts with its previously confirmed phone identity")
			}
		}
		participantID, err = s.ensureParticipantByPhoneTx(ctx, tx, phone, "", "whatsapp")
		if err != nil {
			return err
		}
		if exists && previousID == participantID {
			return nil
		}
		if exists {
			if err := validateOpaqueWhatsAppResolution(ctx, tx, sourceID, previousID, participantID, value); err != nil {
				return err
			}
			if s.FTS5Available() {
				// Older messages may no longer be present in the phone backup.
				// Repair only this opaque sender's index rows, preserving all
				// body/recipient text and stronger attribution on other rows.
				if _, err := tx.ExecContext(ctx, `UPDATE messages_fts SET from_addr=?
					WHERE message_id IN (SELECT id FROM messages WHERE sender_id=? AND source_id=?)`,
					phone, previousID, sourceID); err != nil {
					return fmt.Errorf("resolve historical WhatsApp sender search: %w", err)
				}
			}
			// Use the ordinary identity merge so all journals, projections,
			// curated person checks, and reaction-event references stay intact.
			if err := s.mergeParticipantsTxContext(ctx, tx, previousID, participantID); err != nil {
				return err
			}
			return s.reconcileWhatsAppMergedReactionsTx(ctx, tx, sourceID, participantID, account)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO participant_identifiers
			(participant_id,identifier_type,identifier_value,display_value,is_primary,service_id,scope_kind,scope_value)
			VALUES (?,'whatsapp',?,?,FALSE,(SELECT id FROM communication_services WHERE slug='whatsapp'),'account',?)`,
			participantID, value, jid, account)
		if err != nil {
			return fmt.Errorf("retain WhatsApp LID identifier: %w", err)
		}
		return s.bumpParticipantIdentifierRevision(ctx, tx)
	})
	if err != nil {
		return 0, err
	}
	return participantID, nil
}

func validateOpaqueWhatsAppResolution(ctx context.Context, tx *loggedTx, sourceID, oldID, newID int64, value string) error {
	var otherIdentity bool
	err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(email_address,'')<>'' OR COALESCE(canonical_id,'')<>'' OR
		EXISTS (SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id
			AND NOT (pi.identifier_type='whatsapp' AND pi.identifier_value=?))
		FROM participants p WHERE id=?`, value, oldID).Scan(&otherIdentity)
	if err != nil {
		return err
	}
	if otherIdentity {
		return errors.New("WhatsApp LID resolution conflicts with other participant identity evidence")
	}
	var foreignReference bool
	err = tx.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM messages WHERE sender_id=? AND source_id<>?) OR
		EXISTS (SELECT 1 FROM reactions r JOIN messages m ON m.id=r.message_id WHERE r.participant_id=? AND m.source_id<>?) OR
		EXISTS (SELECT 1 FROM message_recipients r JOIN messages m ON m.id=r.message_id WHERE r.participant_id=? AND m.source_id<>?) OR
		EXISTS (SELECT 1 FROM conversation_participants cp JOIN conversations c ON c.id=cp.conversation_id WHERE cp.participant_id=? AND c.source_id<>?)`,
		oldID, sourceID, oldID, sourceID, oldID, sourceID, oldID, sourceID).Scan(&foreignReference)
	if err != nil {
		return err
	}
	if foreignReference {
		return errors.New("WhatsApp LID participant is also referenced by another source")
	}
	var conflictingRole bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM conversation_participants a JOIN conversation_participants b ON b.conversation_id=a.conversation_id
		WHERE a.participant_id=? AND b.participant_id=? AND a.role<>b.role)`, oldID, newID).Scan(&conflictingRole)
	if err != nil {
		return err
	}
	if conflictingRole {
		return errors.New("WhatsApp LID resolution conflicts with existing conversation roles")
	}
	return nil
}
