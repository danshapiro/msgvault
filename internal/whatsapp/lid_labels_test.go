package whatsapp

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLIDLabelReaderSupportsOptionalAndOlderSchemas(t *testing.T) {
	for _, tc := range []struct{ name, ddl, wantUsername, wantDisplay string }{
		{"absent", "", "", ""},
		{"display_only", `CREATE TABLE lid_display_name(lid_row_id INTEGER PRIMARY KEY,display_name TEXT NOT NULL);INSERT INTO lid_display_name VALUES(4,'+1········11');`, "", "+1········11"},
		{"username", `CREATE TABLE lid_display_name(lid_row_id INTEGER PRIMARY KEY,display_name TEXT NOT NULL,username TEXT);INSERT INTO lid_display_name VALUES(4,'+1········11','sample_user');`, "sample_user", "+1········11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backup := testBackup(t, unresolvedIdentityFixture+tc.ddl)
			db, err := sql.Open("sqlite3", backup)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			labels, err := fetchLIDLabels(t.Context(), db)
			require.NoError(t, err)
			if tc.name == "absent" {
				assert.Empty(t, labels)
				return
			}
			label, ok := labels["22222@lid"]
			require.True(t, ok)
			require.Len(t, labels, 1)
			assert.Equal(t, tc.wantUsername, label.Username)
			assert.Equal(t, tc.wantDisplay, label.DisplayName)
		})
	}
}

func TestLIDLabelReaderReportsMalformedAndCanceledReads(t *testing.T) {
	backup := testBackup(t, unresolvedIdentityFixture+`CREATE TABLE lid_display_name(unexpected INTEGER);`)
	db, err := sql.Open("sqlite3", backup)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = fetchLIDLabels(t.Context(), db)
	require.Error(t, err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = fetchLIDLabels(canceled, db)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLIDResolverUsesSourceLabelsWithoutMergingIdentity(t *testing.T) {
	backup := testBackup(t, unresolvedIdentityFixture+`CREATE TABLE lid_display_name(lid_row_id INTEGER PRIMARY KEY,display_name TEXT NOT NULL,username TEXT);INSERT INTO lid_display_name VALUES(4,'+1········11','sample_user');`)
	db, err := sql.Open("sqlite3", backup)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	require.NoError(t, err)
	resolver, err := newLIDResolver(t.Context(), db, st, source.ID, source.Identifier)
	require.NoError(t, err)
	var jid string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT raw_string FROM jid WHERE _id=4`).Scan(&jid))
	id, err := resolver.ensure(t.Context(), jid, "")
	require.NoError(t, err)
	var name string
	var phone sql.NullString
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name,phone_number FROM participants WHERE id=?`), id).Scan(&name, &phone))
	assert.Equal(t, "@sample_user", name)
	assert.False(t, phone.Valid)
	again, err := resolver.ensure(t.Context(), jid, "")
	require.NoError(t, err)
	assert.Equal(t, id, again)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = resolver.ensure(canceled, jid, "")
	require.ErrorIs(t, err, context.Canceled, "cached observations must still honor cancellation")
}

func TestLIDLabelReaderRejectsConflictingLabelsForSameRawIdentity(t *testing.T) {
	backup := testBackup(t, unresolvedIdentityFixture+`
 CREATE TABLE lid_display_name(lid_row_id INTEGER PRIMARY KEY,display_name TEXT NOT NULL,username TEXT);
 INSERT INTO jid VALUES(6,'22222','lid','22222@lid');
 INSERT INTO lid_display_name VALUES(4,'','sample_user');
 INSERT INTO lid_display_name VALUES(6,'','other_user');`)
	db, err := sql.Open("sqlite3", backup)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = fetchLIDLabels(t.Context(), db)
	require.ErrorContains(t, err, "conflicting display labels")
}
