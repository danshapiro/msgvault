package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWhatsAppObservedLabelsPreserveIdentityAndCuratedNames(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	requirements.NoError(err)
	id, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
	requirements.NoError(err)
	observe := func(username, display string) {
		t.Helper()
		requirements.NoError(st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", username, display))
	}
	read := func() (string, string, sql.NullString) {
		t.Helper()
		var name, display string
		var phone sql.NullString
		requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(p.display_name,''),COALESCE(pi.display_value,''),p.phone_number FROM participants p JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE p.id=?`), id).Scan(&name, &display, &phone))
		return name, display, phone
	}
	observe("sample_user", "+1········11")
	name, display, phone := read()
	assertions.Equal("@sample_user", name)
	assertions.Equal("@sample_user (+1········11)", display)
	assertions.False(phone.Valid)
	revision, err := st.ParticipantDisplayNameRevisionContext(t.Context())
	requirements.NoError(err)
	identifierRevision, err := st.ParticipantIdentifierRevisionContext(t.Context())
	requirements.NoError(err)
	observe("sample_user", "+1········11")
	got, err := st.ParticipantDisplayNameRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(revision, got)
	got, err = st.ParticipantIdentifierRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(identifierRevision, got)
	observe("next_user", "+1········22")
	name, display, _ = read()
	assertions.Equal("@next_user", name)
	assertions.Equal("@next_user (+1········22)", display)
	observe("", "+1········44")
	name, _, _ = read()
	assertions.Equal("@next_user", name, "an older label schema must not replace a known username with a mask")
	observe("", "")
	name, _, _ = read()
	assertions.Equal("@next_user", name, "an older backup without labels cannot clear the current observed label")
	requirements.NoError(st.RepairParticipantDisplayNames(t.Context(), []store.ParticipantDisplayNameRepair{{ParticipantID: id, DisplayName: "Curated Person"}}))
	observe("changed_again", "+1········33")
	name, display, _ = read()
	assertions.Equal("Curated Person", name)
	assertions.Equal("@changed_again (+1········33)", display, "observed labels remain separate from the curated participant name")
	other, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "67890@lid", "")
	requirements.NoError(err)
	requirements.NoError(st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "67890@lid", "changed_again", "+1········33"))
	assertions.NotEqual(id, other, "identical usernames and masks never merge distinct source identities")
}

func TestWhatsAppObservedLabelsDoNotRenamePhoneOrCuratedPerson(t *testing.T) {
	for _, mode := range []string{"phone", "person"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
			requirements.NoError(err)
			id, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
			requirements.NoError(err)
			requirements.NoError(st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", "sample_user", ""))
			if mode == "phone" {
				known, err := st.EnsureParticipantByPhone(t.Context(), "+15550002222", "Phone Contact", "whatsapp")
				requirements.NoError(err)
				id, err = st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "+15550002222")
				requirements.NoError(err)
				assertions.Equal(known, id)
			} else {
				_, _, err := st.CreatePersonFromParticipantContext(t.Context(), id)
				requirements.NoError(err)
			}
			requirements.NoError(st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", "new_user", ""))
			var name string
			requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name FROM participants WHERE id=?`), id).Scan(&name))
			want := "@sample_user"
			if mode == "phone" {
				want = "Phone Contact"
			}
			assertions.Equal(want, name)
		})
	}
}

func TestWhatsAppObservedLabelWritesAreCanceledFencedAndAccountScoped(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	requirements.NoError(err)
	_, err = st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
	requirements.NoError(err)
	run, err := st.StartSyncContext(t.Context(), source.ID, "whatsapp_import")
	requirements.NoError(err)
	scoped := st.ScopedToSync(source.ID, run)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	requirements.ErrorIs(scoped.ObserveWhatsAppLIDLabelContext(canceled, source.ID, source.Identifier, "12345@lid", "sample_user", ""), context.Canceled)
	requirements.ErrorContains(scoped.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, "+15550009999", "12345@lid", "sample_user", ""), "different source account")
	other, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550003333")
	requirements.NoError(err)
	requirements.ErrorContains(scoped.ObserveWhatsAppLIDLabelContext(t.Context(), other.ID, other.Identifier, "12345@lid", "sample_user", ""), "is scoped to source")
	requirements.NoError(st.FailSyncContext(t.Context(), run, "superseded"))
	_, err = st.StartSyncContext(t.Context(), source.ID, "whatsapp_import")
	requirements.NoError(err)
	requirements.ErrorIs(scoped.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", "sample_user", ""), store.ErrSyncRunSuperseded)
	var name string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT display_name FROM participants`).Scan(&name))
	assertions.Empty(name)
}

func TestWhatsAppMaskedLabelsStaySeparateAcrossAccounts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	requirements.NoError(err)
	second, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550002222")
	requirements.NoError(err)
	var ids []int64
	for _, source := range []*store.Source{first, second} {
		id, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
		requirements.NoError(err)
		requirements.NoError(st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", "", "+1········11"))
		ids = append(ids, id)
	}
	assertions.NotEqual(ids[0], ids[1])
	requirements.NoError(st.ObserveWhatsAppLIDLabelContext(t.Context(), first.ID, first.Identifier, "12345@lid", "sample_user", "+1········11"))
	for i, id := range ids {
		var name string
		var phone sql.NullString
		requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name,phone_number FROM participants WHERE id=?`), id).Scan(&name, &phone))
		want := "+1········11"
		if i == 0 {
			want = "@sample_user"
		}
		assertions.Equal(want, name)
		assertions.False(phone.Valid)
	}
}
