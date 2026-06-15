//go:build sqlite_vec

package embed

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func TestDefaultChunkingPolicy(t *testing.T) {
	policy := DefaultChunkingPolicy(2000)

	assert.Equal(t, 2000, policy.MaxInputChars)
	assert.Equal(t, 64, policy.MaxChunksPerMessage)
	assert.Equal(t, 66, policy.OverlapRunes())
	assert.Equal(t, 2000*64*16, policy.MaxBodyRunes())
}

func TestPrepareMessageInputs_HTMLFallbackAndEmpty(t *testing.T) {
	preprocess := PreprocessConfig{CollapseWhitespace: true}

	htmlOnly := MessageInput{
		ID:       1,
		Subject:  "Fallback",
		BodyHTML: "<p>Hello <b>world</b></p>",
	}
	got := PrepareMessageInputs(htmlOnly, preprocess, DefaultChunkingPolicy(2000))
	wantText, wantBodyTruncated := Preprocess(htmlOnly.Subject, mime.StripHTML(htmlOnly.BodyHTML), 0, preprocess)

	assert.False(t, got.Empty)
	assert.Equal(t, wantText, got.Text)
	assert.Equal(t, utf8.RuneCountInString(wantText), got.SourceRunes)
	assert.Equal(t, wantBodyTruncated, got.BodyTruncated)
	require.Len(t, got.Chunks, 1)
	assert.Equal(t, wantText, got.Chunks[0].Text)

	empty := MessageInput{
		ID: 2,
		BodyText: `
		
		`,
	}
	gotEmpty := PrepareMessageInputs(empty, preprocess, DefaultChunkingPolicy(2000))

	assert.True(t, gotEmpty.Empty)
	assert.Empty(t, gotEmpty.Chunks)
}

func TestPrepareMessageInputs_HonorsPolicyCapAndDropsTail(t *testing.T) {
	policy := ChunkingPolicy{MaxInputChars: 100, MaxChunksPerMessage: 3}
	src := MessageInput{
		ID:       1,
		Subject:  "cap",
		BodyText: strings.Repeat("a", 340),
	}

	got := PrepareMessageInputs(src, PreprocessConfig{}, policy)

	assert.False(t, got.Empty)
	assert.True(t, got.TailDropped)
	require.Len(t, got.Chunks, 3)
	assert.Equal(t, 0, got.Chunks[0].ChunkIndex)
	assert.Equal(t, 2, got.Chunks[2].ChunkIndex)
	assert.LessOrEqual(t, got.TotalChunkRunes(), 300)
}

func TestPrepareMessageInputs_CountsRunesWithEmoji(t *testing.T) {
	text := "hi🙂there🙂"
	got := PrepareMessageInputs(
		MessageInput{ID: 1, BodyText: text},
		PreprocessConfig{},
		ChunkingPolicy{MaxInputChars: 100, MaxChunksPerMessage: 3},
	)

	require.NotEmpty(t, got.Chunks)
	assert.Equal(t, utf8.RuneCountInString(text), got.SourceRunes)
	assert.Equal(t, utf8.RuneCountInString(got.Chunks[0].Text), got.Chunks[0].Runes)
}
