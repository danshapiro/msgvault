package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func TestRunEmbeddingsEstimate_PrintsChunkAndStorageSummary(t *testing.T) {
	dataDir, db := newEmbeddingEstimateArchive(t)
	seedEstimateMessage(t, db, 1, "email", "", "short-email", "", false, false)
	seedEstimateMessage(t, db, 2, "email", "", strings.Repeat("x-", 2500), "", false, false)
	seedEstimateMessage(t, db, 3, "sms", "", "sms-body", "", false, false)

	oldCfg := cfg
	oldDimension := embeddingsEstimateDimension
	t.Cleanup(func() {
		cfg = oldCfg
		embeddingsEstimateDimension = oldDimension
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Vector: vector.Config{
			Enabled: true,
			Embeddings: vector.EmbeddingsConfig{
				Model:         "embeddinggemma",
				Dimension:     768,
				BatchSize:     32,
				MaxInputChars: 2000,
			},
		},
	}

	var stdout bytes.Buffer
	cmd := &cobra.Command{Use: "estimate"}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetContext(context.Background())

	requirepkg.NoError(t, runEmbeddingsEstimate(cmd, nil), "runEmbeddingsEstimate")

	out := stdout.String()
	assertpkg.Contains(t, out, "Embedding estimate")
	assertpkg.Contains(t, out, "model: embeddinggemma")
	assertpkg.Contains(t, out, "dimension: 768")
	assertpkg.Contains(t, out, "candidate messages: 3")
	assertpkg.Contains(t, out, "embeddable messages: 3")
	assertpkg.Contains(t, out, "estimated chunks: 5")
	assertpkg.Contains(t, out, "estimated embed requests: 1")
	assertpkg.Contains(t, out, "estimated raw vector bytes: 15360")
	assertpkg.Contains(t, out, "  email: candidates=2 embeddable=2 chunks=4 capped=0 empty=0")
	assertpkg.Contains(t, out, "  sms: candidates=1 embeddable=1 chunks=1 capped=0 empty=0")
}

func TestRunEmbeddingsEstimate_AllowsDimensionFlagWithoutVectorEnabled(t *testing.T) {
	dataDir, db := newEmbeddingEstimateArchive(t)
	seedEstimateMessage(t, db, 1, "email", "Synthetic subject", strings.Repeat("x-", 2500), "", false, false)

	oldCfg := cfg
	oldDimension := embeddingsEstimateDimension
	t.Cleanup(func() {
		cfg = oldCfg
		embeddingsEstimateDimension = oldDimension
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}

	var stdout bytes.Buffer
	cmd := &cobra.Command{Use: "estimate"}
	cmd.Flags().IntVar(&embeddingsEstimateDimension, "dimension", 0, "Override the embedding dimension for raw byte estimates")
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetContext(context.Background())
	requirepkg.NoError(t, cmd.Flags().Set("dimension", "768"), "set dimension flag")

	requirepkg.NoError(t, runEmbeddingsEstimate(cmd, nil), "runEmbeddingsEstimate")
	out := stdout.String()
	assertpkg.Contains(t, out, "dimension: 768")
	assertpkg.Contains(t, out, "max_input_chars: 2000")
	assertpkg.Contains(t, out, "candidate messages: 1")
	assertpkg.Contains(t, out, "embeddable messages: 1")
	assertpkg.Contains(t, out, "estimated chunks: 3")
	assertpkg.Contains(t, out, "estimated embed requests: 1")
	assertpkg.Contains(t, out, "estimated raw vector bytes: 9216")
	assertpkg.Contains(t, out, "  email: candidates=1 embeddable=1 chunks=3 capped=0 empty=0")
}

func TestRunEmbeddingsEstimate_DimensionOverrideDoesNotLeakAcrossRepeatedRuns(t *testing.T) {
	dataDir, db := newEmbeddingEstimateArchive(t)
	seedEstimateMessage(t, db, 1, "email", "Synthetic subject", "synthetic body", "", false, false)

	oldCfg := cfg
	oldDimension := embeddingsEstimateDimension
	flag := embeddingsEstimateCmd.Flags().Lookup("dimension")
	requirepkg.NotNil(t, flag)
	oldFlagValue := flag.Value.String()
	oldFlagChanged := flag.Changed
	oldOut := embeddingsEstimateCmd.OutOrStdout()
	oldErr := embeddingsEstimateCmd.ErrOrStderr()
	oldCtx := embeddingsEstimateCmd.Context()
	t.Cleanup(func() {
		cfg = oldCfg
		embeddingsEstimateDimension = oldDimension
		requirepkg.NoError(t, flag.Value.Set(oldFlagValue))
		flag.Changed = oldFlagChanged
		embeddingsEstimateCmd.SetOut(oldOut)
		embeddingsEstimateCmd.SetErr(oldErr)
		embeddingsEstimateCmd.SetContext(oldCtx)
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}

	var first bytes.Buffer
	embeddingsEstimateCmd.SetOut(&first)
	embeddingsEstimateCmd.SetErr(&first)
	embeddingsEstimateCmd.SetContext(context.Background())
	requirepkg.NoError(t, embeddingsEstimateCmd.Flags().Set("dimension", "768"), "set dimension flag")

	requirepkg.NoError(t, runEmbeddingsEstimate(embeddingsEstimateCmd, nil), "first runEmbeddingsEstimate")
	assertpkg.Contains(t, first.String(), "dimension: 768")

	var second bytes.Buffer
	embeddingsEstimateCmd.SetOut(&second)
	embeddingsEstimateCmd.SetErr(&second)
	embeddingsEstimateCmd.SetContext(context.Background())

	requirepkg.NoError(t, runEmbeddingsEstimate(embeddingsEstimateCmd, nil), "second runEmbeddingsEstimate")
	assertpkg.Contains(t, second.String(), "dimension: unavailable (set vector.embeddings.dimension or --dimension)")
	assertpkg.Contains(t, second.String(), "estimated raw vector bytes: unavailable (dimension not configured)")
}

func TestEstimateEmbeddingsHonorsLiveRowsAndMessageTypeScope(t *testing.T) {
	_, db := newEmbeddingEstimateArchive(t)
	seedEstimateMessage(t, db, 1, "email", "Synthetic email", "email body", "", false, false)
	seedEstimateMessage(t, db, 2, "sms", "", "sms body", "", false, false)
	seedEstimateMessage(t, db, 3, "mms", "", "mms body", "", true, false)
	seedEstimateMessage(t, db, 4, "sms", "", "deleted from source", "", false, true)

	estimate, err := estimateEmbeddings(t.Context(), db, embeddingEstimateConfig{
		Dimension:     768,
		BatchSize:     32,
		MaxInputChars: 2000,
		BuildScope:    vector.NewBuildScope([]string{"sms"}),
	})
	requirepkg.NoError(t, err, "estimateEmbeddings")

	requirepkg.Equal(t, int64(1), estimate.CandidateMessages)
	requirepkg.Equal(t, int64(1), estimate.EmbeddableMessages)
	requirepkg.Contains(t, estimate.ByType, "sms")
	requirepkg.Len(t, estimate.ByType, 1)
	assertpkg.Equal(t, int64(1), estimate.ByType["sms"].Candidates)
	assertpkg.Equal(t, int64(1), estimate.ByType["sms"].Embeddable)
}

func newEmbeddingEstimateArchive(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "msgvault.db")

	st, err := store.Open(dbPath)
	requirepkg.NoError(t, err, "open store")
	requirepkg.NoError(t, st.InitSchema(), "init schema")

	db := st.DB()
	t.Cleanup(func() { requirepkg.NoError(t, st.Close(), "close store") })
	return dataDir, db
}

func seedEstimateMessage(t *testing.T, db *sql.DB, id int64, messageType, subject, bodyText, bodyHTML string, deletedAt, deletedFromSourceAt bool) {
	t.Helper()

	sourceID := id
	conversationID := id
	sourceIdentifier := fmt.Sprintf("source-%d@example.test", id)
	sourceMessageID := fmt.Sprintf("message-%d", id)
	sourceConversationID := fmt.Sprintf("conversation-%d", id)

	_, err := db.Exec(`INSERT INTO sources (id, source_type, identifier, display_name) VALUES (?, 'gmail', ?, ?)`,
		sourceID, sourceIdentifier, fmt.Sprintf("Synthetic Source %d", id))
	requirepkg.NoError(t, err, "insert source")

	_, err = db.Exec(`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES (?, ?, ?, 'email_thread', ?)`,
		conversationID, sourceID, sourceConversationID, fmt.Sprintf("Synthetic Conversation %d", id))
	requirepkg.NoError(t, err, "insert conversation")

	var deletedAtValue any
	if deletedAt {
		deletedAtValue = "2026-01-01 00:00:00"
	}
	var deletedFromSourceValue any
	if deletedFromSourceAt {
		deletedFromSourceValue = "2026-01-02 00:00:00"
	}

	_, err = db.Exec(`
		INSERT INTO messages (
			id, conversation_id, source_id, source_message_id, message_type, subject, snippet, sent_at,
			deleted_at, deleted_from_source_at
		) VALUES (?, ?, ?, ?, ?, ?, '', '2026-01-01 12:00:00', ?, ?)
	`, id, conversationID, sourceID, sourceMessageID, messageType, subject, deletedAtValue, deletedFromSourceValue)
	requirepkg.NoError(t, err, "insert message")

	_, err = db.Exec(`INSERT INTO message_bodies (message_id, body_text, body_html) VALUES (?, ?, ?)`,
		id, bodyText, bodyHTML)
	requirepkg.NoError(t, err, "insert message body")
}
