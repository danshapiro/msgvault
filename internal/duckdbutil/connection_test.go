package duckdbutil

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuilderErrorPreservesCause(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	policy := BuilderPolicyWithOverrides(filepath.Join(t.TempDir(), "spill"), BuilderOverrides{
		MemoryLimit: "1MiB", Threads: 1, MaxTempDirectorySize: "8MB",
	})
	db, err := Open(t.Context(), policy)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(db.Close()) })
	_, oom := db.ExecContext(t.Context(), `CREATE TABLE too_large AS
		SELECT i, repeat('x', 10000) || i::VARCHAR AS payload FROM range(10000) t(i)`)
	requirements.Error(oom)
	requirements.True(IsOutOfMemory(oom))
	for _, cause := range []error{oom, errors.Join(context.Canceled, oom)} {
		got := BuilderError(cause, policy)
		requirements.ErrorIs(got, cause)
		assertions.Contains(got.Error(), "analytics.builder_memory_limit=1MiB")
		assertions.Contains(got.Error(), "analytics.builder_threads=1")
		assertions.Contains(got.Error(), "analytics.builder_temp_limit=8MB")
		assertions.Contains(got.Error(), "msgvault build-cache")
		assertions.Same(got, BuilderError(got, BuilderPolicy(t.TempDir())),
			"nested build paths must retain the actual failing connection's limits")
	}
	for _, cause := range []error{nil, context.Canceled, errors.New("unrelated error")} {
		assertions.Equal(cause, BuilderError(cause, policy))
	}
}

func TestBuilderPolicyDefaults(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tempDir := filepath.Join(t.TempDir(), "spill")

	policy := BuilderPolicy(tempDir)

	requirements.True(ValidSize(policy.MemoryLimit))
	if policy.MemoryLimit != "2GB" {
		budget, err := strconv.ParseUint(strings.TrimSuffix(policy.MemoryLimit, "B"), 10, 64)
		requirements.NoError(err)
		assertions.GreaterOrEqual(budget, uint64(256<<20))
		assertions.LessOrEqual(budget, uint64(16<<30))
	}
	assertions.Equal(min(runtime.GOMAXPROCS(0), 2), policy.Threads)
	assertions.Equal(tempDir, policy.TempDirectory)
	assertions.Equal("32GB", policy.MaxTempDirectorySize)
}

func TestBuilderPolicyWithOverrides(t *testing.T) {
	assertions := assert.New(t)
	tempDir := filepath.Join(t.TempDir(), "spill")
	policy := BuilderPolicyWithOverrides(tempDir, BuilderOverrides{
		MemoryLimit:          "1536mIb",
		Threads:              3,
		MaxTempDirectorySize: "12gB",
	})

	assertions.Equal("1536mIb", policy.MemoryLimit)
	assertions.Equal(3, policy.Threads)
	assertions.Equal(tempDir, policy.TempDirectory)
	assertions.Equal("12gB", policy.MaxTempDirectorySize)

	defaultThreads := BuilderPolicyWithOverrides(tempDir, BuilderOverrides{Threads: 0})
	assertions.Equal(min(runtime.GOMAXPROCS(0), 2), defaultThreads.Threads)
}

func TestPolicyAppliesEffectiveSettings(t *testing.T) {
	for _, test := range []struct {
		name    string
		policy  func(string) Policy
		ordered bool
	}{
		{"interactive default", InteractivePolicy, true},
		{"interactive override", func(dir string) Policy {
			return InteractivePolicyWithOverrides(dir, InteractiveOverrides{MemoryLimit: "384MB", Threads: 3, MaxTempDirectorySize: "1GB"})
		}, true},
		{"builder default", BuilderPolicy, false},
		{"builder override", func(dir string) Policy {
			return BuilderPolicyWithOverrides(dir, BuilderOverrides{MemoryLimit: "1536MB", Threads: 3, MaxTempDirectorySize: "12GB"})
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			tempDir := filepath.Join(t.TempDir(), "spill")
			policy := test.policy(tempDir)
			db, err := Open(t.Context(), policy)
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(db.Close()) })
			requirements.NoError(db.PingContext(t.Context()))
			assertions.DirExists(tempDir)
			// Read effective settings on the actual connection; DuckDB renders
			// byte sizes truncated to one decimal in the chosen binary unit.
			var threads, asyncThreads int
			var preserveOrder bool
			var spillDir string
			var memory, temp string
			requirements.NoError(db.QueryRowContext(t.Context(), `SELECT current_setting('threads'), current_setting('async_threads'),
				current_setting('preserve_insertion_order'), current_setting('temp_directory'),
				current_setting('memory_limit'), current_setting('max_temp_directory_size')`).
				Scan(&threads, &asyncThreads, &preserveOrder, &spillDir, &memory, &temp))
			assertions.Equal(policy.Threads, threads)
			assertions.Equal(policy.Threads, asyncThreads)
			assertions.Equal(test.ordered, preserveOrder)
			assertions.Equal(tempDir, spillDir)
			assertions.InDelta(policyBytes(t, policy.MemoryLimit), policyBytes(t, memory), policyBytes(t, "0.1 "+strings.Fields(memory)[1]))
			assertions.InDelta(policyBytes(t, policy.MaxTempDirectorySize), policyBytes(t, temp), policyBytes(t, "0.1 "+strings.Fields(temp)[1]))
		})
	}
}

func TestPolicyRejectsIncompleteOrUnsafeSettings(t *testing.T) {
	valid := Policy{
		MemoryLimit:          "512MB",
		Threads:              1,
		TempDirectory:        filepath.Join(t.TempDir(), "spill"),
		MaxTempDirectorySize: "2GB",
	}
	tests := []struct {
		name   string
		mutate func(*Policy)
	}{
		{name: "zero threads", mutate: func(policy *Policy) { policy.Threads = 0 }},
		{name: "empty memory limit", mutate: func(policy *Policy) { policy.MemoryLimit = "" }},
		{name: "relative temp directory", mutate: func(policy *Policy) { policy.TempDirectory = "spill" }},
		{name: "empty spill limit", mutate: func(policy *Policy) { policy.MaxTempDirectorySize = "" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := valid
			test.mutate(&policy)
			db, err := Open(t.Context(), policy)
			if db != nil {
				require.NoError(t, db.Close())
			}
			require.Error(t, err)
		})
	}
}

// DuckDB renders byte settings with a rounded binary suffix, while policy
// inputs also accept decimal suffixes.
func policyBytes(t *testing.T, value string) float64 {
	t.Helper()
	parts := regexp.MustCompile(`(?i)^([0-9.]+)\s*([kmgt]?i?b)$`).FindStringSubmatch(value)
	require.Len(t, parts, 3)
	number, err := strconv.ParseFloat(parts[1], 64)
	require.NoError(t, err)
	unit, ok := map[string]float64{"B": 1, "MB": 1e6, "GB": 1e9, "MIB": 1 << 20, "GIB": 1 << 30}[strings.ToUpper(parts[2])]
	require.True(t, ok, "unexpected byte unit %s", value)
	return number * unit
}
