package whatsapp

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// A backup can refer to the same LID thousands of times. Resolve each distinct
// identity observation once; failures are cached too so a conflict cannot
// partially resolve merely because another row appears later in the import.
type lidResolver struct {
	store    *store.Store
	sourceID int64
	account  string
	seen     map[string]lidResolution
}

type lidResolution struct {
	id  int64
	err error
}

func (r *lidResolver) ensure(ctx context.Context, jid, phone string) (int64, error) {
	if !strings.HasSuffix(jid, "@lid") {
		return r.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
	}
	key := jid + "\x00" + phone
	if previous, ok := r.seen[key]; ok {
		return previous.id, previous.err
	}
	id, err := r.store.EnsureWhatsAppLIDParticipantContext(ctx, r.sourceID, r.account, jid, phone)
	r.seen[key] = lidResolution{id, err}
	return id, err
}
