package pst

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractRTFText(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"formatting and paragraphs", `{\rtf1\ansi This is \b bold\b0  text.\par Next\tab cell}`, "This is bold text.\nNext\tcell"},
		{"windows-1252 escaped characters", `{\rtf1\ansi\ansicpg1252 Caf\'e9}`, "Café"},
		{"windows-1251 escaped characters", `{\rtf1\ansi\ansicpg1251 \'cf\'f0\'e8\'e2\'e5\'f2}`, "Привет"},
		{"unicode and surrogate pair", `{\rtf1\ansi \u26085?\u26412? \u-10179?\u-8704?}`, "日本 😀"},
		{"formatting destinations and hidden text", `{\rtf1\ansi{\fonttbl{\f0 Arial;}}{\info{\title Secret}}{\*\unknown not body}one {\v hidden}\v0  two {\pict 001122}three}`, "one  two three"},
		{"unicode in hidden and ignorable groups", `{\rtf1\ansi {\v\uc0\u83\u101\u99\u114\u101\u116}{\*\unknown\uc0\u72\u105}visible}`, "visible"},
		{"terminal padding", "{\\rtf1\\ansi readable}\x00\x00\r\n", "readable"},
		{"font charset overrides document charset", `{\rtf1\ansi\ansicpg1252\deff0{\fonttbl{\f0\fcharset204 Font;}}\'cf\'f0\'e8\'e2\'e5\'f2}`, "Привет"},
		{"escaped literals", `{\rtf1\ansi \{one\} \\ \~\_}`, "{one} \\ \u00a0\u2011"},
		{"binary data is not text", "{\\rtf1\\ansi first{\\*\\blob\\bin4 " + "}{\\x" + "}last}", "firstlast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractRTFText([]byte(tc.input), 4096)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtractRTFTextRejectsUnsafeInput(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"not RTF", `plain text`},
		{"unclosed group", `{\rtf1\ansi body`},
		{"invalid hex", `{\rtf1\ansi \'xz}`},
		{"truncated binary", `{\rtf1\ansi \bin20 xx}`},
		{"unsupported charset", `{\rtf1\ansi\ansicpg99999 bad \'e9}`},
		{"unpaired surrogate", `{\rtf1\ansi \u-10179?x}`},
		{"HTML encapsulation", `{\rtf1\ansi\fromhtml1{\*\htmltag <p>hidden</p>}text}`},
		{"alternate unicode branch", `{\rtf1\ansi{\upr{ansi}{\*\ud unicode}}}`},
		{"symbol font mapping", `{\rtf1\ansi\deff0{\fonttbl{\f0\fcharset2 Symbol;}}abc}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractRTFText([]byte(tc.input), 4096)
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
	_, err := extractRTFText([]byte(`{\rtf1\ansi `+strings.Repeat("x", 256)+`}`), 128)
	require.Error(t, err)
}

func FuzzExtractRTFText(f *testing.F) {
	f.Add([]byte(`{\rtf1\ansi ordinary \u233? text}`))
	f.Add([]byte(`{\rtf1{\*\unknown hidden}\bin4 }{\x}`))
	f.Add([]byte{0, 0xff, '{', '\\'})
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := extractRTFText(data, 4096)
		if err != nil {
			assert.Empty(t, got)
			return
		}
		assert.LessOrEqual(t, len(got), 4096)
	})
}
