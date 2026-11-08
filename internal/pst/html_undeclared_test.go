package pst

import (
	"strings"
	"testing"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractMessage_UndeclaredHTMLCharset(t *testing.T) {
	lateUTF8 := "<p>" + strings.Repeat("a", 1100) + " café 日本語</p>"
	require.Greater(t, strings.Index(lateUTF8, "café"), 1024)
	for _, tc := range []struct {
		name     string
		source   []byte
		want     string
		preserve bool
	}{
		{"UTF-8 beyond sniffing prefix", []byte(lateUTF8), lateUTF8, true},
		{"UTF-8 within sniffing prefix", []byte("<p>café</p>"), "<p>café</p>", true},
		{"legacy byte", []byte("<p>caf\xe9</p>"), "<p>café</p>", true},
		{"ASCII only", []byte("<p>plain text</p>"), "<p>plain text</p>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
				{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: tc.source},
			}}}
			entry := ExtractMessage(msg, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.BodyError)
			assert.Equal(t, tc.want, entry.BodyHTML)
			if tc.preserve {
				require.NotNil(t, entry.RawHTMLAttachment)
				assert.Equal(t, tc.source, entry.RawHTMLAttachment.Content)
			} else {
				assert.Nil(t, entry.RawHTMLAttachment)
			}
		})
	}
}
