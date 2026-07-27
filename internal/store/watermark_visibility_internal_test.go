package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgreSQLVisibilityFloor pins what the bound does when PostgreSQL stops
// showing it every backend.
//
// pg_stat_activity redacts other roles' rows unless the reader is a superuser
// or a member of pg_read_all_stats. A redacted backend cannot hold the bound
// back, so without this floor the bound would quietly return to the clock — the
// exact behaviour the commit bound exists to replace, arrived at by a filter
// matching nothing rather than by a decision. The floor turns that into a
// stalled feed, which is visible.
func TestPostgreSQLVisibilityFloor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }
	floor := func(d *PostgreSQLDialect, bound, start time.Time, redacted int) time.Time {
		t.Helper()
		got, err := d.visibilityFloor(bound, start, redacted)
		require.NoError(err)
		return got
	}

	t.Run("full visibility passes the live bound through", func(t *testing.T) {
		d := &PostgreSQLDialect{}
		assert.Equal(at(5), floor(d, at(5), at(6), 0))
		assert.Equal(at(9), floor(d, at(9), at(10), 0),
			"a later call with full visibility keeps advancing")
	})

	t.Run("a redacted writer caps the bound at the last fully visible one", func(t *testing.T) {
		d := &PostgreSQLDialect{}
		floor(d, at(5), at(6), 0)
		assert.Equal(at(5), floor(d, at(30), at(31), 1),
			"the live bound reached %s while a writer this role cannot see may have "+
				"been writing since before that; the feed must stop where its "+
				"knowledge stopped", at(30))
	})

	t.Run("the cap never widens a tighter live bound", func(t *testing.T) {
		d := &PostgreSQLDialect{}
		floor(d, at(20), at(21), 0)
		assert.Equal(at(8), floor(d, at(8), at(9), 2),
			"a visible writer holding the bound at %s must still win: the floor is a "+
				"ceiling on trust, not a floor on the answer", at(8))
	})

	t.Run("visibility recovering resumes normal service", func(t *testing.T) {
		d := &PostgreSQLDialect{}
		floor(d, at(5), at(6), 0)
		floor(d, at(30), at(31), 1)
		assert.Equal(at(40), floor(d, at(40), at(41), 0),
			"once every backend is visible again the bound is trustworthy again")
	})

	t.Run("never having had visibility refuses the page", func(t *testing.T) {
		d := &PostgreSQLDialect{}
		_, err := d.visibilityFloor(at(5), at(6), 3)
		require.Error(err,
			"with a writer this role cannot see and no fully-visible reading ever "+
				"taken there is nothing to fall back to; publishing the live bound "+
				"anyway steps the consumer's cursor over that writer's change and "+
				"loses it for good")
		assert.Contains(err.Error(), "pg_read_all_stats",
			"the refusal must name the grant that ends it")
	})

	t.Run("a refusal does not poison the floor", func(t *testing.T) {
		d := &PostgreSQLDialect{}
		_, err := d.visibilityFloor(at(5), at(6), 1)
		require.Error(err)
		assert.Equal(at(7), floor(d, at(7), at(8), 0),
			"once the invisible writer's transaction ends the feed must serve again; "+
				"a refusal is a pause, not a wedge")
	})
}
