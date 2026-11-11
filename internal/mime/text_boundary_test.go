package mime

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func textBoundaryFixture(contentType, encoding, body string) []byte {
	return []byte("From: sender@example.test\r\nTo: recipient@example.test\r\n" +
		"Subject: extra text boundary\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: " + contentType + "; charset=utf-8; boundary=unused\r\n" +
		"Content-Transfer-Encoding: " + encoding + "\r\n\r\n" + body + "\r\n" +
		"--outer\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=example.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nAAH+/w==\r\n--outer--\r\n")
}

func TestParseUnusedTextBoundaryPreservesContent(t *testing.T) {
	for name, tc := range map[string]struct {
		contentType string
		encoding    string
		body        string
	}{
		"plain":                    {"text/plain", "8bit", "Plain body with café"},
		"delimiter prefix is text": {"text/plain", "8bit", "--unusedSuffix is ordinary text"},
		"html":                     {"text/html", "8bit", "<p>HTML body with café</p>"},
		"quoted printable":         {"text/plain", "quoted-printable", "A=3DB and caf=C3=A9"},
		"base64":                   {"text/plain", "base64", base64.StdEncoding.EncodeToString([]byte("Body with café"))},
	} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			raw := textBoundaryFixture(tc.contentType, tc.encoding, tc.body)
			original := bytes.Clone(raw)
			valid := bytes.ReplaceAll(raw, []byte("; boundary=unused"), nil)
			want, err := Parse(valid)
			require.NoError(err)
			require.False(want.Incomplete)

			got, fingerprints, err := ParseWithRecoveryAndPartFingerprints(raw, "")
			require.NoError(err)
			assert.False(got.Incomplete)
			assert.Equal(want.BodyText, got.BodyText)
			assert.Equal(want.BodyHTML, got.BodyHTML)
			assert.Equal(want.Attachments, got.Attachments)
			assert.Equal(original, raw)
			assert.Len(got.Errors, len(want.Errors)+1)
			assert.Contains(got.Errors, "[W] Data contains boundary marker: content is text or data but has a boundary marker")
			assert.Len(fingerprints, 3)
		})
	}
}

func TestParseUnusedTextBoundaryUsesOwnPartScope(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	raw := textBoundaryFixture("text/plain", "8bit", "Ordinary body")
	// A sibling's content can legitimately quote the unused delimiter. It does
	// not make the text leaf's parameter an actual multipart declaration.
	raw = bytes.Replace(raw, []byte("Content-Transfer-Encoding: base64\r\n\r\nAAH+/w=="),
		[]byte("Content-Transfer-Encoding: 8bit\r\n\r\n--unused"), 1)
	got, err := Parse(raw)
	require.NoError(err)
	assert.False(got.Incomplete)
	require.Len(got.Attachments, 1)
	assert.Equal([]byte("--unused"), got.Attachments[0].Content)
}

func TestParseUnusedTextBoundariesRequireEveryLeafSafe(t *testing.T) {
	for _, safe := range []bool{true, false} {
		t.Run(map[bool]string{true: "both safe", false: "one contains delimiter"}[safe], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			raw := textBoundaryFixture("text/plain", "8bit", "Plain body")
			html := "<p>HTML body</p>"
			if !safe {
				html += "\r\n--unused--"
			}
			raw = bytes.Replace(raw, []byte("--outer\r\nContent-Type: application/octet-stream"),
				[]byte("--outer\r\nContent-Type: text/html; boundary=unused\r\n\r\n"+html+
					"\r\n--outer\r\nContent-Type: application/octet-stream"), 1)
			got, err := Parse(raw)
			require.NoError(err)
			assert.Equal(!safe, got.Incomplete)
			require.Len(got.Errors, 2)
			for _, problem := range got.Errors {
				assert.Equal(safe, strings.HasPrefix(problem, "[W]"))
			}
		})
	}
}

func TestParseUnusedTextBoundaryDoesNotDowngradeOtherSevereError(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	raw := textBoundaryFixture("text/plain", "8bit", "Ordinary body")
	raw = bytes.Replace(raw, []byte("AAH+/w=="), []byte("a==="), 1)
	got, err := Parse(raw)
	require.NoError(err)
	assert.True(got.Incomplete)
	assert.Contains(got.Errors, "[E] Data contains boundary marker: content is text or data but has a boundary marker")
	assert.Contains(strings.Join(got.Errors, "\n"), "[E] Malformed Base64")
}

func TestParseUnusedTextBoundaryInNestedMultipart(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	raw := textBoundaryFixture("text/plain", "8bit", "Nested body")
	raw = bytes.Replace(raw, []byte("--outer\r\nContent-Type: text/plain"),
		[]byte("--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain"), 1)
	raw = bytes.Replace(raw, []byte("Nested body\r\n--outer"), []byte("Nested body\r\n--inner--\r\n--outer"), 1)
	got, err := Parse(raw)
	require.NoError(err)
	assert.False(got.Incomplete)
	assert.Equal("Nested body", got.BodyText)
	require.Len(got.Attachments, 1)
	assert.Equal([]byte{0, 1, 254, 255}, got.Attachments[0].Content)

	// A recovered parent boundary cannot prove that its nested scope closed.
	unclosed := bytes.Replace(raw, []byte("--inner--\r\n"), nil, 1)
	got, err = Parse(unclosed)
	require.NoError(err)
	assert.True(got.Incomplete)
}

func TestParseUnsafeTextBoundaryRemainsIncomplete(t *testing.T) {
	for name, raw := range map[string][]byte{
		"raw delimiter":                     textBoundaryFixture("text/plain", "8bit", "Before\r\n--unused\r\nAfter"),
		"closing delimiter with whitespace": textBoundaryFixture("text/html", "8bit", "Before\r\n--unused-- \t\r\nAfter"),
		"quoted printable delimiter":        textBoundaryFixture("text/plain", "quoted-printable", "Before\r\n=2D=2Dunused\r\nAfter"),
		"base64 delimiter":                  textBoundaryFixture("text/plain", "base64", base64.StdEncoding.EncodeToString([]byte("Before\r\n--unused--\r\nAfter"))),
		"unknown encoding":                  textBoundaryFixture("text/plain", "x-unknown", "Ordinary body"),
		"invalid base64":                    textBoundaryFixture("text/plain", "base64", "a"),
		"duplicate content type": bytes.Replace(textBoundaryFixture("text/plain", "8bit", "Body"),
			[]byte("boundary=unused\r\n"), []byte("boundary=unused\r\nContent-Type: text/html\r\n"), 1),
		"duplicate transfer encoding": bytes.Replace(textBoundaryFixture("text/plain", "8bit", "Body"),
			[]byte("Content-Transfer-Encoding: 8bit\r\n"), []byte("Content-Transfer-Encoding: 8bit\r\nContent-Transfer-Encoding: base64\r\n"), 1),
		"unclosed multipart": []byte(strings.ReplaceAll(string(textBoundaryFixture("text/plain", "8bit", "Body")), "--outer--\r\n", "")),
	} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			got, fingerprints, err := ParseWithRecoveryAndPartFingerprints(raw, "")
			require.NoError(err)
			assert.True(got.Incomplete)
			assert.Empty(fingerprints)
		})
	}
}
