package mbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseFromSeparatorDateStrict_ParsesKnownTZAbbrev(t *testing.T) {
	line := "From a@b Mon Jan 1 00:00:00 PST 2024"
	ts, ok := ParseFromSeparatorDateStrict(line)
	require.True(t, ok, "expected ok")
	require.Equal(t, "2024-01-01T08:00:00Z", ts.UTC().Format(time.RFC3339))
}

func TestParseFromSeparatorDateStrict_ParsesKnownTZAbbrevAfterYear(t *testing.T) {
	line := "From a@b Mon Jan 1 00:00:00 2024 PST"
	ts, ok := ParseFromSeparatorDateStrict(line)
	require.True(t, ok, "expected ok")
	require.Equal(t, "2024-01-01T08:00:00Z", ts.UTC().Format(time.RFC3339))
}

func TestParseFromSeparatorDateStrict_ParsesNumericOffset(t *testing.T) {
	line := "From a@b Mon Jan 1 00:00:00 -0700 2024"
	ts, ok := ParseFromSeparatorDateStrict(line)
	require.True(t, ok, "expected ok")
	require.Equal(t, "2024-01-01T07:00:00Z", ts.UTC().Format(time.RFC3339))
}

func TestParseFromSeparatorDateStrict_RejectsUnknownTZAbbrev(t *testing.T) {
	line := "From a@b Mon Jan 1 00:00:00 FOO 2024"
	_, ok := ParseFromSeparatorDateStrict(line)
	require.False(t, ok, "expected not ok")
	lineAfterYear := "From a@b Mon Jan 1 00:00:00 2024 FOO"
	_, ok = ParseFromSeparatorDateStrict(lineAfterYear)
	require.False(t, ok, "expected not ok")
	_, ok = ParseFromSeparatorDate(line)
	require.True(t, ok, "expected permissive ParseFromSeparatorDate to accept line for separator detection")
}

func TestParseFromSeparatorDate_QuotedSender(t *testing.T) {
	line := `From "Example Person <person@example.test>" Mon Jan 1 00:00:00 2024`
	for _, parse := range []func(string) (time.Time, bool){ParseFromSeparatorDate, ParseFromSeparatorDateStrict} {
		got, ok := parse(line)
		require.True(t, ok)
		require.Equal(t, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), got)
		_, ok = parse(`From "unclosed sender Mon Jan 1 00:00:00 2024`)
		require.False(t, ok)
		_, ok = parse(`From "quoted"@example.test Mon Jan 1 00:00:00 2024`)
		require.True(t, ok, "single-token quoted sender remains supported")
		_, ok = parse(`From "Example \"Person\" <person@example.test>" Mon Jan 1 00:00:00 2024`)
		require.True(t, ok, "escaped quotes in a multiword sender")
		_, ok = parse(`From "Example Person"junk Mon Jan 1 00:00:00 2024`)
		require.False(t, ok, "a quoted sender must be followed by whitespace")
	}
}
