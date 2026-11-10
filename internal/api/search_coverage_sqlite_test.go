//go:build fts5 && sqlite_vec

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestHandleSearchReportsAcceleratorPath(t *testing.T) {
	t.Parallel()
	vectorCfg := vector.Config{
		Enabled:    true,
		Embeddings: vector.EmbeddingsConfig{Model: "test-model", Dimension: 2, MaxInputChars: 1000},
	}
	backend := newRealCoverageBackend(t, vectorCfg, "active-matching")
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{
		ExpectedFingerprint: vectorCfg.GenerationFingerprint(),
	})
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}, Vector: vectorCfg},
		Store:  &mockStore{}, HybridEngine: engine, Backend: backend,
		VectorCfg: vectorCfg, VectorStatus: VectorStatusReady, Logger: testLogger(),
	})
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/search?q=hello&mode=vector", nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body hybridSearchResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "exact", body.Accelerator)
}

func TestSearchCoverageRealGenerationStateMatrix(t *testing.T) {
	t.Parallel()
	vectorCfg := vector.Config{
		Enabled: true,
		Embeddings: vector.EmbeddingsConfig{
			Model: "test-model", Dimension: 2, MaxInputChars: 1000,
		},
	}
	tests := []struct {
		name         string
		generation   string
		vectorStatus VectorStatus
		wantStatus   SearchCoverageStatus
		wantEligible int64
		wantEmbedded int64
		wantActions  []SearchCoverageAction
	}{
		{
			name: "no generation", wantStatus: SearchCoverageUnavailable,
			wantActions: []SearchCoverageAction{SearchCoverageActionRetry, SearchCoverageActionBuildIndex},
		},
		{
			name: "matching first build", generation: "building-matching",
			vectorStatus: VectorStatusInitializing,
			wantStatus:   SearchCoverageInitializing,
		},
		{
			name: "different first build", generation: "building-different",
			vectorStatus: VectorStatusInitializing,
			wantStatus:   SearchCoverageUnavailable,
			wantActions:  []SearchCoverageAction{SearchCoverageActionRetry},
		},
		{
			name: "matching active", generation: "active-matching",
			wantStatus: SearchCoverageReady, wantEligible: 2, wantEmbedded: 2,
		},
		{
			name: "stale active", generation: "active-different",
			wantStatus: SearchCoverageStale, wantEligible: 2, wantEmbedded: 2,
			wantActions: []SearchCoverageAction{SearchCoverageActionBuildIndex},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			vectorStatus := tt.vectorStatus
			if vectorStatus == "" {
				vectorStatus = VectorStatusReady
			}
			backend := newRealCoverageBackend(t, vectorCfg, tt.generation)
			cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}, Vector: vectorCfg}
			srv := NewServerWithOptions(ServerOptions{
				Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Engine: newExploreDuckDBFixture(t),
				Backend: backend, VectorCfg: vectorCfg, VectorStatus: vectorStatus, Logger: testLogger(),
			})

			response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{
				"filters":[{"dimension":"source","values":["1"]}]
			}`)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var body SearchCoverageResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(tt.wantStatus, body.Status)
			assert.Equal(tt.wantEligible, body.EligibleCount)
			assert.Equal(tt.wantEmbedded, body.EmbeddedCount)
			assert.ElementsMatch(tt.wantActions, body.Actions)
		})
	}
}

func newRealCoverageBackend(t *testing.T, cfg vector.Config, generationState string) *sqlitevec.Backend {
	t.Helper()
	ctx := t.Context()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "coverage@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "coverage", "email_thread", "Coverage")
	require.NoError(t, err)
	for _, sourceMessageID := range []string{"one", "two"} {
		_, err := st.UpsertMessage(&store.Message{
			SourceID: source.ID, SourceMessageID: sourceMessageID,
			ConversationID: conversationID, MessageType: "email",
			Subject: sql.NullString{String: sourceMessageID, Valid: true},
		})
		require.NoError(t, err)
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 2, MainDB: st.DB(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	if generationState == "" {
		return backend
	}
	fingerprint := cfg.GenerationFingerprint()
	if generationState == "building-different" || generationState == "active-different" {
		fingerprint = "different"
	}
	generationID, err := backend.CreateGeneration(ctx, "test-model", 2, fingerprint)
	require.NoError(t, err)
	if generationState == "building-matching" || generationState == "building-different" {
		return backend
	}
	require.NoError(t, backend.Upsert(ctx, generationID, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0}},
		{MessageID: 2, Vector: []float32{0, 1}},
	}))
	require.NoError(t, st.SetEmbedGen(ctx, []int64{1, 2}, int64(generationID)))
	require.NoError(t, backend.ActivateGeneration(ctx, generationID, true))
	return backend
}

func TestCoverageLargeNativeOrderedStream(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	const n = int64(815019)
	engine, dir := newGeneratedExploreFixture(t, n)
	for _, size := range []int{512, 257} {
		for repeat := range 2 {
			started := time.Now()
			next, calls := int64(1), 0
			result, err := engine.ExploreCoverage(t.Context(),
				query.ExploreCoverageRequest{BatchSize: size}, func(ids []int64) error {
					requirements.NotEmpty(ids)
					requirements.LessOrEqual(len(ids), size)
					for _, id := range ids {
						requirements.Equal(next, id)
						next++
					}
					calls++
					return nil
				})
			requirements.NoError(err)
			assertions.Equal(n, result.EligibleCount)
			assertions.Equal(n+1, next)
			assertions.Greater(calls, 1000)
			assertions.NotEmpty(result.CacheRevision)
			t.Logf("coverage batch=%d repeat=%d scanned %d in %s", size, repeat, n, time.Since(started))
		}
	}
	t.Run("source and failures", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		next := int64(2)
		result, err := engine.ExploreCoverage(t.Context(), query.ExploreCoverageRequest{
			Context: query.Context{SourceIDs: []int64{1}}, BatchSize: 257,
		}, func(ids []int64) error {
			for _, id := range ids {
				requirements.Equal(next, id)
				next += 2
			}
			return nil
		})
		requirements.NoError(err)
		assertions.Equal(n/2, result.EligibleCount)
		assertions.Equal(n+1, next)
		visitErr, calls := errors.New("synthetic intersection failure"), 0
		_, err = engine.ExploreCoverage(t.Context(), query.ExploreCoverageRequest{BatchSize: 512}, func([]int64) error {
			calls++
			if calls == 4 {
				return visitErr
			}
			return nil
		})
		requirements.ErrorIs(err, visitErr)
		assertions.Equal(4, calls)
	})
	t.Run("cancellation releases reader", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		_, err := engine.ExploreCoverage(ctx, query.ExploreCoverageRequest{BatchSize: 512}, func([]int64) error {
			calls++
			if calls == 3 {
				cancel()
			}
			return nil
		})
		requirements.ErrorIs(err, context.Canceled)
		assertions.Equal(3, calls)
		page, err := engine.Explore(t.Context(), query.ExploreRequest{Page: query.PageSpec{Limit: 1}})
		requirements.NoError(err)
		assertions.Len(page.Rows, 1)
	})
	t.Run("date pages and groups", func(t *testing.T) { checkGeneratedExplorePages(t, engine, n) })
	t.Run("real vector API", func(t *testing.T) { checkGeneratedCoverageAPI(t, dir, n) })
}

// Walk decimal prefixes in key order. The oracle retains only the requested
// 3,000 rows and never sorts or stores the full generated population.
func generatedExplorePageIDs(n int64, limit int) []int64 {
	ids := make([]int64, 0, limit)
	for second := int64(4095); second >= 0 && len(ids) < limit; second-- {
		var walk func(int64)
		walk = func(prefix int64) {
			if prefix > n || len(ids) == limit {
				return
			}
			if prefix%4096 == second {
				ids = append(ids, prefix)
			}
			for digit := range int64(10) {
				walk(prefix*10 + digit)
			}
		}
		for first := int64(1); first < 10; first++ {
			walk(first)
		}
	}
	return ids
}

func checkGeneratedExplorePages(t *testing.T, engine *query.DuckDBEngine, n int64) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	want := generatedExplorePageIDs(n, 3000)
	requirements.Len(want, 3000)
	for range 2 {
		for offset := 0; offset < len(want); offset += 1000 {
			page, err := engine.Explore(t.Context(), query.ExploreRequest{Page: query.PageSpec{Offset: offset, Limit: 1000}})
			requirements.NoError(err)
			requirements.Len(page.Rows, 1000)
			assertions.Equal(n, page.TotalCount)
			for i, row := range page.Rows {
				requirements.NotNil(row.AnchorMessageID)
				assertions.Equal(want[offset+i], *row.AnchorMessageID)
				assertions.Equal(base.Add(time.Duration(want[offset+i]%4096)*time.Second), row.OccurredAt)
			}
		}
		for _, dimension := range []string{"day", "source"} {
			groups, err := engine.ExploreGroups(t.Context(), query.ExploreGroupRequest{
				Dimension: dimension, Timezone: "UTC", Sort: query.SortSpec{Field: "key", Direction: "asc"}, Page: query.PageSpec{Limit: 10},
			})
			requirements.NoError(err)
			if dimension == "day" {
				requirements.Len(groups.Rows, 1)
				assertions.Equal("2026-07-18", groups.Rows[0].Key)
				assertions.Equal(n, groups.Rows[0].Count)
			} else {
				requirements.Len(groups.Rows, 2)
				for i, row := range groups.Rows {
					assertions.Equal(strconv.Itoa(i+1), row.Key)
					assertions.Equal([]string{"archive-a@example.com", "archive-b@example.com"}[i], row.Label)
					assertions.Equal([]int64{n / 2, n - n/2}[i], row.Count)
				}
			}
		}
		// These adjacent seconds lie beyond the incomplete final cycle and
		// therefore give both sources exactly the same count.
		after, before := base.Add(4092*time.Second), base.Add(4094*time.Second)
		tied, err := engine.ExploreGroups(t.Context(), query.ExploreGroupRequest{
			Dimension: "source", Explore: query.ExploreRequest{Context: query.Context{After: &after, Before: &before}},
			Sort: query.SortSpec{Field: "count", Direction: "desc"}, Page: query.PageSpec{Limit: 10},
		})
		requirements.NoError(err)
		requirements.Len(tied.Rows, 2)
		for i, row := range tied.Rows {
			assertions.Equal(strconv.Itoa(i+1), row.Key)
			assertions.Equal(n/4096, row.Count)
		}
	}
}

// All counting is performed by the real SQLite/vector backend. The forwarding
// hook records bounded calls and injects faults only after a successful count.
type observedCoverageBackend struct {
	*sqlitevec.Backend

	calls    int
	maxBatch int
	after    func(int) error
}

func (b *observedCoverageBackend) EmbeddedMessageCountForIDs(ctx context.Context, gen vector.GenerationID, ids []int64) (int64, error) {
	count, err := b.Backend.EmbeddedMessageCountForIDs(ctx, gen, ids)
	if err != nil {
		return 0, err
	}
	b.calls++
	b.maxBatch = max(b.maxBatch, len(ids))
	if b.after != nil {
		if err := b.after(b.calls); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func checkGeneratedCoverageAPI(t *testing.T, dir string, n int64) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	ctx := t.Context()
	// Add two sentinel rows to the same committed fixture: source-deleted mail
	// and an out-of-embedding-scope message. Internally deleted n+3 is absent
	// from Parquet, exactly as it is with the real archive builder.
	db, err := sql.Open("duckdb", "")
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(db.Close()) })
	_, err = db.ExecContext(ctx, fmt.Sprintf(`COPY (
SELECT i::BIGINT AS id, 1::BIGINT AS source_id, 'sentinel-' || i AS source_message_id,
101::BIGINT AS conversation_id, 'Sentinel' AS subject, '' AS snippet,
TIMESTAMP '2026-07-18' AS sent_at, 100::BIGINT AS size_estimate, false AS has_attachments,
0::INTEGER AS attachment_count, CASE WHEN i = %d THEN TIMESTAMP '2026-07-19' ELSE NULL::TIMESTAMP END AS deleted_from_source_at,
1::BIGINT AS sender_id, NULL::BIGINT AS owner_participant_id,
CASE WHEN i = %d THEN 'sms' ELSE 'email' END AS message_type, NULL::VARCHAR AS list_id,
false AS is_from_me, 2026 AS year, 7 AS month, false AS archive_only
FROM range(%d, %d) AS r(i)) TO '%s' (FORMAT PARQUET)`, n+1, n+2, n+1, n+3, filepath.ToSlash(filepath.Join(dir, "messages/year=2026/sentinels.parquet"))))
	requirements.NoError(err)
	ensureIdentityCacheFixtureDatasets(t, db, dir, identityCacheFixtureOptions{})
	requirements.NoError(db.Close())
	state, err := query.ReadCacheSyncState(dir)
	requirements.NoError(err)
	state.LastMessageID = n + 3
	state.PublishedAt = state.PublishedAt.Add(time.Second)
	state.DatasetFingerprint, err = query.CacheDatasetFingerprint(dir)
	requirements.NoError(err)
	publish := func() {
		encoded, err := json.Marshal(state)
		requirements.NoError(err)
		requirements.NoError(os.WriteFile(query.CacheStatePath(dir), encoded, 0o600))
	}
	publish()
	// A fresh connection binds the updated Parquet file list, without another
	// copy of the large dataset or an archive rebuild.
	engine, err := query.NewDuckDBEngine(ctx, dir, "", nil, query.DuckDBOptions{Threads: 4})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	st := testutil.NewSQLiteTestStore(t)
	for _, account := range []string{"archive-a@example.com", "archive-b@example.com"} {
		_, err := st.GetOrCreateSource("gmail", account)
		requirements.NoError(err)
	}
	conversationID, err := st.EnsureConversationWithType(1, "coverage", "email_thread", "Coverage")
	requirements.NoError(err)
	started := time.Now()
	tx, err := st.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `WITH RECURSIVE ids(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM ids WHERE i < ?)
INSERT INTO messages(id, source_id, source_message_id, conversation_id, message_type, sent_at, deleted_from_source_at, deleted_at)
SELECT i, CASE WHEN i <= ? THEN 1+i%2 ELSE 1 END, 'synthetic-' || i, ?,
CASE WHEN i = ? THEN 'sms' ELSE 'email' END, '2026-07-18 00:00:00',
CASE WHEN i = ? THEN '2026-07-19' END, CASE WHEN i = ? THEN '2026-07-19' END FROM ids`, n+3, n, conversationID, n+2, n+1, n+3)
	requirements.NoError(err)
	requirements.NoError(tx.Commit())
	t.Logf("bulk main SQLite population %d in %s", n+3, time.Since(started))
	cfg := vector.Config{Enabled: true, Embeddings: vector.EmbeddingsConfig{Model: "test-model", Dimension: 2, MaxInputChars: 1000}, Embed: vector.EmbedConfig{Scope: vector.EmbedScopeConfig{MessageTypes: []string{"email"}}}}
	vectorBackend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 2, MainDB: st.DB()})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(vectorBackend.Close()) })
	old, err := vectorBackend.CreateGeneration(ctx, "test-model", 2, "old-generation")
	requirements.NoError(err)
	requirements.NoError(vectorBackend.Upsert(ctx, old, []vector.Chunk{{MessageID: 1026, Vector: []float32{1, 0}}}))
	requirements.NoError(st.SetEmbedGen(ctx, []int64{1026}, int64(old)))
	requirements.NoError(vectorBackend.ActivateGeneration(ctx, old, true))
	gen, err := vectorBackend.CreateGeneration(ctx, "test-model", 2, cfg.GenerationFingerprint())
	requirements.NoError(err)
	for start := int64(1); start <= 1024; start += 512 {
		chunks, ids := make([]vector.Chunk, 0, 512), make([]int64, 0, 512)
		for id := start; id < start+512; id++ {
			chunks = append(chunks, vector.Chunk{MessageID: id, Vector: []float32{1, 0}})
			ids = append(ids, id)
		}
		requirements.NoError(vectorBackend.Upsert(ctx, gen, chunks))
		requirements.NoError(st.SetEmbedGen(ctx, ids, int64(gen)))
	}
	var extras []vector.Chunk
	for id := int64(1); id <= 16; id++ {
		extras = append(extras, vector.Chunk{MessageID: id, Vector: []float32{1, 0}}, vector.Chunk{MessageID: id, ChunkIndex: 1, Vector: []float32{0, 1}})
	}
	for id := n + 1; id <= n+3; id++ {
		extras = append(extras, vector.Chunk{MessageID: id, Vector: []float32{1, 0}})
	}
	requirements.NoError(vectorBackend.Upsert(ctx, gen, extras))
	requirements.NoError(st.SetEmbedGen(ctx, []int64{1025, 1026, n + 1, n + 2, n + 3}, int64(gen))) // 1025 has no vector; 1026 has only an old-generation vector
	requirements.NoError(vectorBackend.ActivateGeneration(ctx, gen, true))
	backend := &observedCoverageBackend{Backend: vectorBackend}
	newServer := func() *Server {
		return NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}, Vector: cfg}, Store: &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend, VectorCfg: cfg, VectorStatus: VectorStatusReady, Logger: testLogger()})
	}
	srv := newServer()
	read := func(t *testing.T, srv *Server, request string) SearchCoverageResponse {
		t.Helper()
		requirements := require.New(t)
		response := postExploreJSON(t, srv, "/api/v1/search/coverage", request)
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
		var body SearchCoverageResponse
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
		return body
	}
	for _, test := range []struct {
		request            string
		eligible, embedded int64
	}{
		{`{}`, n, 1024}, {`{"filters":[{"dimension":"source","values":["1"]}]}`, n / 2, 512},
	} {
		first := read(t, srv, test.request)
		assertions.Equal(test.eligible, first.EligibleCount)
		assertions.Equal(test.embedded, first.EmbeddedCount)
		assertions.InDelta(0.1, first.Percentage, 0.0001)
		assertions.Equal(SearchCoverageIncomplete, first.Status)
		requirements.NotNil(first.VectorGeneration)
		assertions.Equal(int64(gen), *first.VectorGeneration)
		assertions.Equal(cfg.GenerationFingerprint(), first.VectorFingerprint)
		assertions.Equal(state.Revision(), first.CacheRevision)
		calls := backend.calls
		for range 2 {
			assertions.Equal(first, read(t, srv, test.request))
			assertions.Equal(calls, backend.calls)
		}
	}
	assertions.Greater(backend.calls, 1000)
	assertions.Equal(vector.FilteredCoverageBatchSize, backend.maxBatch)
	assertions.LessOrEqual(backend.maxBatch, 512)
	calls := backend.calls
	state.PublishedAt = state.PublishedAt.Add(time.Second)
	publish()
	refreshed := read(t, srv, `{}`)
	assertions.Equal(state.Revision(), refreshed.CacheRevision)
	assertions.Equal(n, refreshed.EligibleCount)
	assertions.Equal(int64(1024), refreshed.EmbeddedCount)
	assertions.Greater(backend.calls, calls)
	t.Run("backend failure is never cached", func(t *testing.T) {
		assertions := assert.New(t)
		backend.calls = 0
		backend.after = func(call int) error {
			if call == 4 {
				return errors.New("synthetic backend failure")
			}
			return nil
		}
		failedServer := newServer()
		failed := read(t, failedServer, `{}`)
		assertions.Equal(SearchCoverageUnavailable, failed.Status)
		assertions.Zero(failed.EmbeddedCount)
		assertions.Equal(4, backend.calls)
		backend.after = nil
		assertions.Equal(int64(1024), read(t, failedServer, `{}`).EmbeddedCount)
		assertions.Greater(backend.calls, 4)
	})
	t.Run("generation switch rejects partial count", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		next, err := vectorBackend.CreateGeneration(ctx, "test-model", 2, cfg.GenerationFingerprint())
		requirements.NoError(err)
		backend.calls = 0
		backend.after = func(call int) error {
			if call == 4 {
				return vectorBackend.ActivateGeneration(ctx, next, true)
			}
			return nil
		}
		response := postExploreJSON(t, newServer(), "/api/v1/search/coverage", `{}`)
		assertions.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
		var body ErrorResponse
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
		assertions.Equal("vector_generation_changed", body.Error)
		backend.after = nil
	})
}
