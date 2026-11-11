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
			assertions := assert.New(t)
			requirements := require.New(t)
			backup := testBackup(t, unresolvedIdentityFixture+tc.ddl)
			db, err := sql.Open("sqlite3", backup)
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(db.Close()) })
			labels, err := fetchLIDLabels(t.Context(), db)
			requirements.NoError(err)
			if tc.name == "absent" {
				assertions.Empty(labels)
				return
			}
			label, ok := labels["22222@lid"]
			requirements.True(ok)
			requirements.Len(labels, 1)
			assertions.Equal(tc.wantUsername, label.Username)
			assertions.Equal(tc.wantDisplay, label.DisplayName)
		})
	}
}

func TestLIDLabelReaderReportsMalformedAndCanceledReads(t *testing.T) {
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture+`CREATE TABLE lid_display_name(unexpected INTEGER);`)
	db, err := sql.Open("sqlite3", backup)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(db.Close()) })
	_, err = fetchLIDLabels(t.Context(), db)
	requirements.Error(err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = fetchLIDLabels(canceled, db)
	requirements.ErrorIs(err, context.Canceled)
}

func TestLIDResolverUsesSourceLabelsWithoutMergingIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture+`CREATE TABLE lid_display_name(lid_row_id INTEGER PRIMARY KEY,display_name TEXT NOT NULL,username TEXT);INSERT INTO lid_display_name VALUES(4,'+1········11','sample_user');`)
	db, err := sql.Open("sqlite3", backup)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(db.Close()) })
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "whatsapp", "+15550001111")
	requirements.NoError(err)
	resolver, err := newLIDResolver(t.Context(), db, st, source.ID, source.Identifier)
	requirements.NoError(err)
	var jid string
	requirements.NoError(db.QueryRowContext(t.Context(), `SELECT raw_string FROM jid WHERE _id=4`).Scan(&jid))
	id, err := resolver.ensure(t.Context(), jid, "")
	requirements.NoError(err)
	var name string
	var phone sql.NullString
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name,phone_number FROM participants WHERE id=?`), id).Scan(&name, &phone))
	assertions.Equal("@sample_user", name)
	assertions.False(phone.Valid)
	again, err := resolver.ensure(t.Context(), jid, "")
	requirements.NoError(err)
	assertions.Equal(id, again)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = resolver.ensure(canceled, jid, "")
	requirements.ErrorIs(err, context.Canceled, "cached observations must still honor cancellation")
}

func TestLIDLabelReaderRejectsConflictingLabelsForSameRawIdentity(t *testing.T) {
	requirements := require.New(t)
	backup := testBackup(t, unresolvedIdentityFixture+`
 CREATE TABLE lid_display_name(lid_row_id INTEGER PRIMARY KEY,display_name TEXT NOT NULL,username TEXT);
 INSERT INTO jid VALUES(6,'22222','lid','22222@lid');
 INSERT INTO lid_display_name VALUES(4,'','sample_user');
 INSERT INTO lid_display_name VALUES(6,'','other_user');`)
	db, err := sql.Open("sqlite3", backup)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(db.Close()) })
	_, err = fetchLIDLabels(t.Context(), db)
	requirements.ErrorContains(err, "conflicting display labels")
}
