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
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	require.NoError(t, err)
	id, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
	require.NoError(t, err)
	observe := func(username, display string) {
		t.Helper()
		require.NoError(t, st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", username, display))
	}
	read := func() (string, string, sql.NullString) {
		t.Helper()
		var name, display string
		var phone sql.NullString
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(p.display_name,''),COALESCE(pi.display_value,''),p.phone_number FROM participants p JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE p.id=?`), id).Scan(&name, &display, &phone))
		return name, display, phone
	}
	observe("sample_user", "+1········11")
	name, display, phone := read()
	assert.Equal(t, "@sample_user", name)
	assert.Equal(t, "@sample_user (+1········11)", display)
	assert.False(t, phone.Valid)
	revision, err := st.ParticipantDisplayNameRevisionContext(t.Context())
	require.NoError(t, err)
	identifierRevision, err := st.ParticipantIdentifierRevisionContext(t.Context())
	require.NoError(t, err)
	observe("sample_user", "+1········11")
	got, err := st.ParticipantDisplayNameRevisionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, revision, got)
	got, err = st.ParticipantIdentifierRevisionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, identifierRevision, got)
	observe("next_user", "+1········22")
	name, display, _ = read()
	assert.Equal(t, "@next_user", name)
	assert.Equal(t, "@next_user (+1········22)", display)
	observe("", "+1········44")
	name, _, _ = read()
	assert.Equal(t, "@next_user", name, "an older label schema must not replace a known username with a mask")
	observe("", "")
	name, _, _ = read()
	assert.Equal(t, "@next_user", name, "an older backup without labels cannot clear the current observed label")
	require.NoError(t, st.RepairParticipantDisplayNames(t.Context(), []store.ParticipantDisplayNameRepair{{ParticipantID: id, DisplayName: "Curated Person"}}))
	observe("changed_again", "+1········33")
	name, display, _ = read()
	assert.Equal(t, "Curated Person", name)
	assert.Equal(t, "@changed_again (+1········33)", display, "observed labels remain separate from the curated participant name")
	other, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "67890@lid", "")
	require.NoError(t, err)
	require.NoError(t, st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "67890@lid", "changed_again", "+1········33"))
	assert.NotEqual(t, id, other, "identical usernames and masks never merge distinct source identities")
}

func TestWhatsAppObservedLabelsDoNotRenamePhoneOrCuratedPerson(t *testing.T) {
	for _, mode := range []string{"phone", "person"} {
		t.Run(mode, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
			require.NoError(t, err)
			id, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
			require.NoError(t, err)
			require.NoError(t, st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", "sample_user", ""))
			if mode == "phone" {
				known, err := st.EnsureParticipantByPhone(t.Context(), "+15550002222", "Phone Contact", "whatsapp")
				require.NoError(t, err)
				id, err = st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "+15550002222")
				require.NoError(t, err)
				assert.Equal(t, known, id)
			} else {
				_, _, err := st.CreatePersonFromParticipantContext(t.Context(), id)
				require.NoError(t, err)
			}
			require.NoError(t, st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", "new_user", ""))
			var name string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name FROM participants WHERE id=?`), id).Scan(&name))
			want := "@sample_user"
			if mode == "phone" {
				want = "Phone Contact"
			}
			assert.Equal(t, want, name)
		})
	}
}

func TestWhatsAppObservedLabelWritesAreCanceledFencedAndAccountScoped(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	require.NoError(t, err)
	_, err = st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
	require.NoError(t, err)
	run, err := st.StartSyncContext(t.Context(), source.ID, "whatsapp_import")
	require.NoError(t, err)
	scoped := st.ScopedToSync(source.ID, run)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, scoped.ObserveWhatsAppLIDLabelContext(canceled, source.ID, source.Identifier, "12345@lid", "sample_user", ""), context.Canceled)
	require.ErrorContains(t, scoped.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, "+15550009999", "12345@lid", "sample_user", ""), "different source account")
	other, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550003333")
	require.NoError(t, err)
	require.ErrorContains(t, scoped.ObserveWhatsAppLIDLabelContext(t.Context(), other.ID, other.Identifier, "12345@lid", "sample_user", ""), "is scoped to source")
	require.NoError(t, st.FailSyncContext(t.Context(), run, "superseded"))
	_, err = st.StartSyncContext(t.Context(), source.ID, "whatsapp_import")
	require.NoError(t, err)
	require.ErrorIs(t, scoped.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", "sample_user", ""), store.ErrSyncRunSuperseded)
	var name string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT display_name FROM participants`).Scan(&name))
	assert.Empty(t, name)
}

func TestWhatsAppMaskedLabelsStaySeparateAcrossAccounts(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	require.NoError(t, err)
	second, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550002222")
	require.NoError(t, err)
	var ids []int64
	for _, source := range []*store.Source{first, second} {
		id, err := st.EnsureWhatsAppLIDParticipantContext(t.Context(), source.ID, source.Identifier, "12345@lid", "")
		require.NoError(t, err)
		require.NoError(t, st.ObserveWhatsAppLIDLabelContext(t.Context(), source.ID, source.Identifier, "12345@lid", "", "+1········11"))
		ids = append(ids, id)
	}
	assert.NotEqual(t, ids[0], ids[1])
	require.NoError(t, st.ObserveWhatsAppLIDLabelContext(t.Context(), first.ID, first.Identifier, "12345@lid", "sample_user", "+1········11"))
	for i, id := range ids {
		var name string
		var phone sql.NullString
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name,phone_number FROM participants WHERE id=?`), id).Scan(&name, &phone))
		want := "+1········11"
		if i == 0 {
			want = "@sample_user"
		}
		assert.Equal(t, want, name)
		assert.False(t, phone.Valid)
	}
}
