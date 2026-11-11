package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ObserveWhatsAppLIDLabelContext retains source presentation separately from
// the stable account-scoped identity. It never asserts a phone/person match.
// An empty label from an older snapshot cannot clear an observed name, and a
// participant name changed independently of this source remains authoritative.
func (s *Store) ObserveWhatsAppLIDLabelContext(ctx context.Context, sourceID int64, account, jid, username, displayName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if account == "" || !strings.HasSuffix(jid, "@lid") || strings.TrimSuffix(jid, "@lid") == "" {
		return errors.New("WhatsApp account and full LID are required")
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	displayName = strings.TrimSpace(displayName)
	if username == "" && displayName == "" {
		return nil
	}
	value := whatsAppLIDIdentifier(account, jid)
	return s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		if err := s.lockParticipantDirectoryMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var sourceAccount, sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT identifier,source_type FROM sources WHERE id=?`, sourceID).Scan(&sourceAccount, &sourceType); err != nil {
			return fmt.Errorf("read WhatsApp label source: %w", err)
		}
		if sourceType != "whatsapp" || sourceAccount != account {
			return errors.New("WhatsApp label belongs to a different source account")
		}
		var id int64
		var name, previous, phone, email, canonical string
		var curated bool
		if err := tx.QueryRowContext(ctx, `SELECT p.id,COALESCE(p.display_name,''),COALESCE(pi.display_value,''),COALESCE(p.phone_number,''),COALESCE(p.email_address,''),COALESCE(p.canonical_id,''),EXISTS(SELECT 1 FROM person_participants pp WHERE pp.participant_id=p.id)
   FROM participant_identifiers pi JOIN participants p ON p.id=pi.participant_id WHERE pi.identifier_type='whatsapp' AND pi.identifier_value=?`, value).Scan(&id, &name, &previous, &phone, &email, &canonical, &curated); err != nil {
			return fmt.Errorf("read WhatsApp label owner: %w", err)
		}
		previousPreferred := previous
		if strings.HasPrefix(previous, "@") {
			previousPreferred, _, _ = strings.Cut(previous, " (")
		}
		preferred := displayName
		if username != "" {
			preferred = "@" + username
		} else if strings.HasPrefix(previousPreferred, "@") {
			// Older source schemas and weak snapshots can retain a mask while
			// omitting a username. Keep the stronger observed username.
			preferred = previousPreferred
		}
		observed := preferred
		if strings.HasPrefix(preferred, "@") && displayName != "" && displayName != preferred {
			observed += " (" + displayName + ")"
		}
		if previous != observed {
			if _, err := tx.ExecContext(ctx, `UPDATE participant_identifiers SET display_value=? WHERE identifier_type='whatsapp' AND identifier_value=?`, observed, value); err != nil {
				return fmt.Errorf("retain WhatsApp source label: %w", err)
			}
			if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
				return err
			}
		}
		// Phone/email/canonical identities and promoted people can have names from
		// stronger sources. Labels remain visible on their scoped identifier, but
		// this importer does not rename those participant records.
		sourceOwned := name == "" || (previous != value && previous != jid && name == previousPreferred)
		if curated || phone != "" || email != "" || canonical != "" || !sourceOwned || name == preferred {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE participants SET display_name=?,updated_at=`+s.dialect.Now()+` WHERE id=?`, preferred, id); err != nil {
			return fmt.Errorf("retain WhatsApp participant label: %w", err)
		}
		if err := s.bumpParticipantDisplayNameRevisionContext(ctx, tx); err != nil {
			return err
		}
		return s.invalidateParticipantPersonEnrichmentTx(ctx, tx, id)
	})
}
