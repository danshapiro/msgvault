package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

var embeddingsEstimateDimension int

type embeddingEstimate struct {
	CandidateMessages  int64
	EmbeddableMessages int64
	EstimatedChunks    int64
	EmbedRequests      int64
	CappedMessages     int64
	EmptyMessages      int64
	RawVectorBytes     int64
	ByType             map[string]embeddingEstimateType
}

type embeddingEstimateType struct {
	Candidates int64
	Embeddable int64
	Chunks     int64
	Capped     int64
	Empty      int64
}

type embeddingEstimateConfig struct {
	Model         string
	Dimension     int
	BatchSize     int
	MaxInputChars int
	Preprocess    embed.PreprocessConfig
	BuildScope    vector.BuildScope
}

func runEmbeddingsEstimate(cmd *cobra.Command, _ []string) error {
	defer resetEmbeddingsEstimateFlagState(cmd)

	dimension := cfg.Vector.Embeddings.Dimension
	if cmd != nil {
		if flag := cmd.Flags().Lookup("dimension"); flag != nil && flag.Changed {
			override, err := cmd.Flags().GetInt("dimension")
			if err != nil {
				return fmt.Errorf("read dimension flag: %w", err)
			}
			dimension = override
		}
	}
	if dimension < 0 {
		return fmt.Errorf("dimension must be non-negative, got %d", dimension)
	}

	dbPath, err := cfg.DatabasePath()
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}

	s, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open main db: %w", err)
	}
	defer func() { _ = s.Close() }()

	ecfg := estimateConfigFromRuntime(dimension)
	estimate, err := estimateEmbeddings(cmd.Context(), s.DB(), ecfg)
	if err != nil {
		return err
	}

	printEmbeddingEstimate(cmd.OutOrStdout(), estimate, ecfg)
	return nil
}

func resetEmbeddingsEstimateFlagState(cmd *cobra.Command) {
	embeddingsEstimateDimension = 0
	if cmd == nil {
		return
	}
	flag := cmd.Flags().Lookup("dimension")
	if flag == nil {
		return
	}
	_ = flag.Value.Set("0")
	flag.Changed = false
}

func estimateConfigFromRuntime(dimension int) embeddingEstimateConfig {
	vectorCfg := cfg.Vector
	vectorCfg.ApplyDefaults()
	return embeddingEstimateConfig{
		Model:         vectorCfg.Embeddings.Model,
		Dimension:     dimension,
		BatchSize:     vectorCfg.Embeddings.BatchSize,
		MaxInputChars: vectorCfg.Embeddings.MaxInputChars,
		Preprocess: embed.PreprocessConfig{
			StripQuotes:        vectorCfg.Preprocess.StripQuotesEnabled(),
			StripSignatures:    vectorCfg.Preprocess.StripSignaturesEnabled(),
			StripHTML:          vectorCfg.Preprocess.StripHTMLEnabled(),
			StripBase64:        vectorCfg.Preprocess.StripBase64Enabled(),
			StripURLTracking:   vectorCfg.Preprocess.StripURLTrackingEnabled(),
			CollapseWhitespace: vectorCfg.Preprocess.CollapseWhitespaceEnabled(),
		},
		BuildScope: vectorCfg.Embed.Scope.BuildScope(),
	}
}

func estimateEmbeddings(ctx context.Context, db *sql.DB, ecfg embeddingEstimateConfig) (embeddingEstimate, error) {
	if ecfg.BatchSize <= 0 {
		ecfg.BatchSize = 32
	}

	policy := embed.DefaultChunkingPolicy(ecfg.MaxInputChars)
	estimate := embeddingEstimate{ByType: make(map[string]embeddingEstimateType)}

	query := `
		SELECT
			m.id,
			COALESCE(m.message_type, 'email'),
			COALESCE(m.subject, ''),
			COALESCE(mb.body_text, ''),
			COALESCE(mb.body_html, '')
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE ` + store.LiveMessagesWhere("m", true)
	args := make([]any, 0, len(ecfg.BuildScope.MessageTypes))
	if len(ecfg.BuildScope.MessageTypes) > 0 {
		placeholders := make([]string, len(ecfg.BuildScope.MessageTypes))
		for i, messageType := range ecfg.BuildScope.MessageTypes {
			placeholders[i] = "?"
			args = append(args, messageType)
		}
		query += " AND m.message_type IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY m.id"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return embeddingEstimate{}, fmt.Errorf("query live messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			messageID   int64
			messageType string
			subject     string
			bodyText    string
			bodyHTML    string
		)
		if err := rows.Scan(&messageID, &messageType, &subject, &bodyText, &bodyHTML); err != nil {
			return embeddingEstimate{}, fmt.Errorf("scan message row: %w", err)
		}

		prepared := embed.PrepareMessageInputs(embed.MessageInput{
			ID:       messageID,
			Subject:  subject,
			BodyText: bodyText,
			BodyHTML: bodyHTML,
		}, ecfg.Preprocess, policy)

		estimate.CandidateMessages++
		byType := estimate.ByType[messageType]
		byType.Candidates++

		if prepared.Empty || len(prepared.Chunks) == 0 {
			estimate.EmptyMessages++
			byType.Empty++
			estimate.ByType[messageType] = byType
			continue
		}

		estimate.EmbeddableMessages++
		estimate.EstimatedChunks += int64(len(prepared.Chunks))
		byType.Embeddable++
		byType.Chunks += int64(len(prepared.Chunks))

		if prepared.BodyTruncated || prepared.TailDropped {
			estimate.CappedMessages++
			byType.Capped++
		}

		estimate.ByType[messageType] = byType
	}
	if err := rows.Err(); err != nil {
		return embeddingEstimate{}, fmt.Errorf("iterate message rows: %w", err)
	}

	estimate.EmbedRequests = int64(math.Ceil(float64(estimate.EstimatedChunks) / float64(ecfg.BatchSize)))
	if ecfg.Dimension > 0 {
		estimate.RawVectorBytes = estimate.EstimatedChunks * int64(ecfg.Dimension) * 4
	}

	return estimate, nil
}

func printEmbeddingEstimate(w io.Writer, e embeddingEstimate, ecfg embeddingEstimateConfig) {
	fmt.Fprintln(w, "Embedding estimate")
	if ecfg.Model != "" {
		fmt.Fprintf(w, "model: %s\n", ecfg.Model)
	} else {
		fmt.Fprintln(w, "model: unavailable (vector.embeddings.model not configured)")
	}
	if ecfg.Dimension > 0 {
		fmt.Fprintf(w, "dimension: %d\n", ecfg.Dimension)
	} else {
		fmt.Fprintln(w, "dimension: unavailable (set vector.embeddings.dimension or --dimension)")
	}
	fmt.Fprintf(w, "max_input_chars: %d\n", ecfg.MaxInputChars)
	fmt.Fprintf(w, "batch_size: %d\n", ecfg.BatchSize)
	fmt.Fprintf(w, "candidate messages: %d\n", e.CandidateMessages)
	fmt.Fprintf(w, "embeddable messages: %d\n", e.EmbeddableMessages)
	fmt.Fprintf(w, "estimated chunks: %d\n", e.EstimatedChunks)
	fmt.Fprintf(w, "estimated embed requests: %d\n", e.EmbedRequests)
	if ecfg.Dimension > 0 {
		fmt.Fprintf(w, "estimated raw vector bytes: %d\n", e.RawVectorBytes)
	} else {
		fmt.Fprintln(w, "estimated raw vector bytes: unavailable (dimension not configured)")
	}
	fmt.Fprintf(w, "capped/truncated messages: %d\n", e.CappedMessages)
	fmt.Fprintf(w, "empty/unembeddable messages: %d\n", e.EmptyMessages)
	fmt.Fprintln(w, "note: actual vectors.db will be larger due to SQLite metadata and indexes")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "By message_type:")
	keys := make([]string, 0, len(e.ByType))
	for messageType := range e.ByType {
		keys = append(keys, messageType)
	}
	sort.Strings(keys)
	for _, messageType := range keys {
		row := e.ByType[messageType]
		fmt.Fprintf(w, "  %s: candidates=%d embeddable=%d chunks=%d capped=%d empty=%d\n",
			messageType, row.Candidates, row.Embeddable, row.Chunks, row.Capped, row.Empty)
	}
}
