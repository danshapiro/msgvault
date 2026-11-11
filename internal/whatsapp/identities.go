package whatsapp

import (
	"context"
	"database/sql"
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
	labels   map[string]waLIDLabel
}

type lidResolution struct {
	id  int64
	err error
}

func newLIDResolver(ctx context.Context, db *sql.DB, st *store.Store, sourceID int64, account string) (*lidResolver, error) {
	labels, err := fetchLIDLabels(ctx, db)
	if err != nil {
		return nil, err
	}
	return &lidResolver{store: st, sourceID: sourceID, account: account, seen: make(map[string]lidResolution), labels: labels}, nil
}

func (r *lidResolver) ensure(ctx context.Context, jid, phone string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !strings.HasSuffix(jid, "@lid") {
		return r.store.EnsureParticipantByPhone(ctx, phone, "", "whatsapp")
	}
	key := jid + "\x00" + phone
	if previous, ok := r.seen[key]; ok {
		return previous.id, previous.err
	}
	id, err := r.store.EnsureWhatsAppLIDParticipantContext(ctx, r.sourceID, r.account, jid, phone)
	if label, ok := r.labels[jid]; err == nil && ok {
		err = r.store.ObserveWhatsAppLIDLabelContext(ctx, r.sourceID, r.account, jid, label.Username, label.DisplayName)
	}
	r.seen[key] = lidResolution{id, err}
	return id, err
}
