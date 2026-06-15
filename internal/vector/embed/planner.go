package embed

import (
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/mime"
)

type ChunkingPolicy struct {
	MaxInputChars       int
	MaxChunksPerMessage int
}

func DefaultChunkingPolicy(maxInputChars int) ChunkingPolicy {
	return ChunkingPolicy{
		MaxInputChars:       maxInputChars,
		MaxChunksPerMessage: maxSpansPerMessage,
	}
}

func (p ChunkingPolicy) OverlapRunes() int {
	return chunkOverlapFor(p.MaxInputChars)
}

func (p ChunkingPolicy) MaxBodyRunes() int {
	if p.MaxInputChars <= 0 || p.MaxChunksPerMessage <= 0 {
		return 0
	}
	return p.MaxInputChars * p.MaxChunksPerMessage * rawBodyMultiplier
}

func (p ChunkingPolicy) ChunkText(text string) ([]ChunkSpan, bool) {
	return ChunkText(text, p.MaxInputChars, p.OverlapRunes(), p.MaxChunksPerMessage)
}

type MessageInput struct {
	ID       int64
	Subject  string
	BodyText string
	BodyHTML string
}

type PreparedChunk struct {
	ID         int64
	ChunkIndex int
	Text       string
	Runes      int
	CharStart  int
	CharEnd    int
	Truncated  bool
}

type PreparedMessage struct {
	ID            int64
	Text          string
	SourceRunes   int
	Empty         bool
	BodyTruncated bool
	TailDropped   bool
	Chunks        []PreparedChunk
}

func (m PreparedMessage) TotalChunkRunes() int {
	total := 0
	for _, ch := range m.Chunks {
		total += ch.Runes
	}
	return total
}

func PrepareMessageInputs(src MessageInput, preprocess PreprocessConfig, policy ChunkingPolicy) PreparedMessage {
	body := src.BodyText
	if body == "" && src.BodyHTML != "" {
		body = mime.StripHTML(src.BodyHTML)
	}

	if preprocess.MaxBodyRunes == 0 {
		preprocess.MaxBodyRunes = policy.MaxBodyRunes()
	}

	text, bodyTruncated := Preprocess(src.Subject, body, 0, preprocess)
	prepared := PreparedMessage{
		ID:            src.ID,
		Text:          text,
		SourceRunes:   utf8.RuneCountInString(text),
		BodyTruncated: bodyTruncated,
	}
	if strings.TrimSpace(text) == "" {
		prepared.Empty = true
		return prepared
	}

	spans, tailDropped := policy.ChunkText(text)
	prepared.TailDropped = tailDropped
	prepared.Chunks = make([]PreparedChunk, 0, len(spans))
	for i, sp := range spans {
		runes := sp.CharEnd - sp.CharStart
		prepared.Chunks = append(prepared.Chunks, PreparedChunk{
			ID:         src.ID,
			ChunkIndex: i,
			Text:       sp.Text,
			Runes:      runes,
			CharStart:  sp.CharStart,
			CharEnd:    sp.CharEnd,
			Truncated: bodyTruncated || tailDropped ||
				(policy.MaxInputChars > 0 && runes == policy.MaxInputChars && i < len(spans)-1),
		})
	}

	return prepared
}
