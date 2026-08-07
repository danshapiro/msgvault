package slack

import (
	"os"
	"testing"
	"time"
)

// TestTestClockContract verifies the package clock: anchored at
// testClockBase (noon-of-today UTC by default, or MSGVAULT_TEST_CLOCK_BASE
// when set) and advancing with real elapsed time.
func TestTestClockContract(t *testing.T) {
	if v := os.Getenv("MSGVAULT_TEST_CLOCK_BASE"); v != "" {
		want, err := time.Parse(time.RFC3339, v)
		if err != nil {
			t.Fatalf("MSGVAULT_TEST_CLOCK_BASE is not RFC3339: %v", err)
		}
		if !testClockBase.Equal(want) {
			t.Fatalf("testClockBase = %v, want env override %v", testClockBase, want)
		}
	} else {
		// Noon UTC, and within half a day of the real clock (i.e. today's
		// noon). Checked field-wise rather than against a freshly computed
		// "today" so the assertion itself cannot flake across midnight.
		if testClockBase.Hour() != 12 || testClockBase.Minute() != 0 ||
			testClockBase.Second() != 0 || testClockBase.Nanosecond() != 0 ||
			testClockBase.Location() != time.UTC {
			t.Fatalf("testClockBase = %v, want noon UTC", testClockBase)
		}
		if d := time.Since(testClockBase); d < -13*time.Hour || d > 13*time.Hour {
			t.Fatalf("testClockBase = %v is not noon of today (%v from real now)", testClockBase, d)
		}
	}

	a := testNow()
	b := testNow()
	if b.Before(a) {
		t.Fatalf("testNow went backwards: %v then %v", a, b)
	}
	if a.Before(testClockBase) {
		t.Fatalf("testNow() = %v is before testClockBase %v", a, testClockBase)
	}
}

// processStart pins the moment this test process began; testNow advances
// from testClockBase by real elapsed time so ordering-sensitive code still
// sees a moving clock.
var processStart = time.Now()

// testClockBase anchors every test time read at noon-of-today UTC — outside
// every measured midnight-adjacent failure window — so fixtures and the
// importer's injected clock stay coherent regardless of the wall clock.
// Override with MSGVAULT_TEST_CLOCK_BASE (RFC3339) to reproduce a specific
// window, e.g. MSGVAULT_TEST_CLOCK_BASE=2026-08-07T23:59:30Z.
var testClockBase = func() time.Time {
	if v := os.Getenv("MSGVAULT_TEST_CLOCK_BASE"); v != "" {
		base, err := time.Parse(time.RFC3339, v)
		if err != nil {
			panic("MSGVAULT_TEST_CLOCK_BASE must be RFC3339 (e.g. 2026-08-07T12:00:00Z): " + err.Error())
		}
		return base.UTC()
	}
	y, m, d := time.Now().UTC().Date()
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}()

// testNow is the package test clock: deterministic base, advancing with
// real elapsed time. It is installed as the importer's time source in
// testImporter and used for every fixture timestamp in this package's
// tests.
func testNow() time.Time {
	return testClockBase.Add(time.Since(processStart))
}
