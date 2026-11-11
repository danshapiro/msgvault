package whatsapp

import (
	"context"
	"errors"
	"fmt"
)

// Retain the latest explicit observation per actor, including empty removals.
// The store compares retained timestamps too: older backups and missing rows
// cannot erase newer evidence, even after an opaque identity is resolved.
func (imp *Importer) importReactions(ctx context.Context, messageID int64, reactions []waReaction,
	lids *lidResolver, lidMap map[int64]waLidMapping, selfID int64,
	reportError func(string, error), reportIdentityLimitation func(string, string),
) int64 {
	latest := make(map[int64]waReaction)
	ambiguous := make(map[int64]bool)
	for _, r := range reactions {
		participantID := selfID
		var err error
		switch {
		case r.FromMe.Valid && r.FromMe.Int64 == 1:
			// Explicit source self evidence takes precedence over a missing JID.
		case r.SenderServer.Valid && r.SenderServer.String == "lid":
			phone := resolveLidSender(r.SenderJIDRowID, r.SenderServer.String, lidMap)
			participantID, err = lids.ensure(ctx, r.SenderRawString.String, phone)
			if err == nil && phone == "" {
				reportIdentityLimitation("reaction_phone_unavailable", r.SenderRawString.String)
			}
		case r.SenderUser.Valid && r.SenderUser.String != "":
			phone := normalizePhone(r.SenderUser.String, r.SenderServer.String)
			if phone == "" {
				err = errors.New("reaction sender is not a recognized phone JID")
			} else {
				participantID, err = imp.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
			}
		case r.SenderJIDRowID.Valid && r.SenderJIDRowID.Int64 > 0:
			err = errors.New("reaction sender references a missing identity")
			// NULL and non-positive source sentinels retain the legacy self
			// representation; a dangling positive JID must never become self.
		}
		if err != nil {
			reportError("reaction", fmt.Errorf("retain reaction identity: %w", err))
			continue
		}
		previous, found := latest[participantID]
		if !found || r.Timestamp > previous.Timestamp {
			latest[participantID] = r
			ambiguous[participantID] = false
		} else if r.Timestamp == previous.Timestamp && mapReaction(r) != mapReaction(previous) {
			ambiguous[participantID] = true
		}
	}
	var retained int64
	for participantID, r := range latest {
		if ambiguous[participantID] {
			reportError("reaction_conflict", errors.New("reaction identity has conflicting latest observations"))
			continue
		}
		if err := imp.store.ObserveWhatsAppReactionContext(ctx, messageID, participantID, mapReaction(r), r.Timestamp); err != nil {
			reportError("reaction", fmt.Errorf("retain message reaction: %w", err))
		} else if mapReaction(r) != "" {
			retained++
		}
	}
	return retained
}
