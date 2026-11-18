package mime

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func deepHTMLFixture(attachmentEncoding string, complete bool) ([]byte, string) {
	html := strings.Repeat("<div>", 600) + "Before &amp; after <b>visible</b><script>private script</script><style>private style</style> finish" + strings.Repeat("</div>", 600)
	raw := "From: Example <sender@example.invalid>\r\nTo: Reader <reader@example.invalid>\r\nSubject: Deep HTML\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + html + "\r\n--outer\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=proof.dat\r\nContent-Transfer-Encoding: base64\r\n\r\n" + attachmentEncoding + "\r\n"
	if complete {
		raw += "--outer--\r\n"
	}
	return []byte(raw), html
}

func TestDeepHTMLRecoversProjectionAndPreservesAttachments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw, html := deepHTMLFixture("AAEC//8=", true)
	before := sha256.Sum256(raw)
	msg, err := Parse(raw)
	require.NoError(err)
	require.NotNil(msg)
	assert.False(msg.Incomplete)
	assert.Equal("Before & after visible finish", msg.BodyText)
	assert.Equal(html, msg.BodyHTML)
	assert.Equal(before, sha256.Sum256(raw))
	require.Len(msg.Attachments, 1)
	assert.Equal([]byte{0, 1, 2, 255, 255}, msg.Attachments[0].Content)
	assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte{0, 1, 2, 255, 255})), msg.Attachments[0].ContentHash)
	assert.True(HTMLHasHiddenText(msg.BodyHTML), "deep HTML remains conservatively flagged; projection recovery is not trust")
	assert.Contains(strings.Join(msg.Errors, "\n"), "recovered with existing HTML text projection")
}

func TestDeepHTMLRecoveryKeepsMIMECorruptionIncomplete(t *testing.T) {
	raw, _ := deepHTMLFixture("S", true)
	msg, err := Parse(raw)
	require.NoError(t, err)
	assert.True(t, msg.Incomplete, "HTML recovery cannot waive damaged attachment bytes")
	assert.Contains(t, strings.Join(msg.Errors, "\n"), "Malformed Base64")
}

func TestDeepHTMLRecoveryWithoutAttachments(t *testing.T) {
	body := strings.Repeat("<div>", 600) + "café &lt;literal&gt;\x00 retained" + strings.Repeat("</div>", 600)
	msg := parseEmail(t, emailOptions{ContentType: "text/html; charset=utf-8", Body: body})
	assert.False(t, msg.Incomplete)
	assert.Equal(t, "café <literal>� retained", msg.BodyText)
	assert.Empty(t, msg.Attachments)
}

func TestDeepHTMLRecoveryPreservesExistingWordBoundaries(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"<dl><dt>Username</dt><dd>Alice</dd></dl>", "Username\n\nAlice"},
		{"prefix<pre>code</pre>suffix", "prefix\ncode\nsuffix"},
		{"<table><tr><td>first</td><td>second</td></tr></table>", "first\n\nsecond"},
		{"<head><title>not body</title></head><p>body</p>", "body"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			body := strings.Repeat("<div>", 600) + tc.body + strings.Repeat("</div>", 600)
			msg := parseEmail(t, emailOptions{ContentType: "text/html; charset=utf-8", Body: body})
			assert.False(t, msg.Incomplete)
			assert.Equal(t, tc.want, msg.BodyText)
		})
	}
}
