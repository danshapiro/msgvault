package pst

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf16"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWalkFoldersRecursive_SearchFolderRecord(t *testing.T) {
	t.Run("search folder is skipped", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		folder := pstlib.Folder{
			Identifier:   1827,
			Name:         "All Messages",
			MessageCount: 2178,
		}
		var got []FolderEntry

		err := walkFoldersRecursive(&folder, "ROOT_FOLDER/Search Root", func(entry FolderEntry, _ *pstlib.Folder) error {
			got = append(got, entry)
			return nil
		})
		require.NoError(err)
		assert.Empty(got)

		_, err = folder.GetMessageIterator()
		require.ErrorIs(err, pstlib.ErrMessagesNotFound)
	})

	t.Run("normal folder with same name and count is visited", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		folder := pstlib.Folder{
			Identifier:   1826,
			Name:         "All Messages",
			MessageCount: 2178,
		}
		var got []FolderEntry

		err := walkFoldersRecursive(&folder, "ROOT_FOLDER/Search Root", func(entry FolderEntry, _ *pstlib.Folder) error {
			got = append(got, entry)
			return nil
		})
		require.NoError(err)
		require.Len(got, 1)
		assert.Equal("ROOT_FOLDER/Search Root/All Messages", got[0].Path)
		assert.Equal(int32(2178), got[0].MsgCount)
	})
}

func TestWalkFolders_MatchesLibraryTraversalWithoutSearchFolders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, err := Open(filepath.Join("testdata", "support.pst"))
	require.NoError(err, "Open")
	defer func() { _ = f.Close() }()

	var got []pstlib.Identifier
	err = f.WalkFolders(func(_ FolderEntry, folder *pstlib.Folder) error {
		got = append(got, folder.Identifier)
		return nil
	})
	require.NoError(err, "WalkFolders")

	var all, want []pstlib.Identifier
	err = f.pstFile.WalkFolders(func(folder *pstlib.Folder) error {
		all = append(all, folder.Identifier)
		if folder.Identifier.GetType() != pstlib.IdentifierTypeSearchFolder {
			want = append(want, folder.Identifier)
		}
		return nil
	})
	require.NoError(err, "go-pst WalkFolders")
	require.Less(len(want), len(all))
	assert.Equal(want, got)
}

func TestExtractMessage_MeetingRequest(t *testing.T) {
	unicodeProperty := func(id uint16, value string) pstlib.Property {
		var data []byte
		for _, unit := range utf16.Encode([]rune(value + "\x00")) {
			data = binary.LittleEndian.AppendUint16(data, unit)
		}
		return pstlib.Property{ID: id, Type: pstlib.PropertyTypeString, Data: data}
	}
	filetime := make([]byte, 8)
	binary.LittleEndian.PutUint64(filetime, uint64((time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC).Unix()+11644473600)*10000000))
	props := []pstlib.Property{
		unicodeProperty(0x001a, "IPM.Schedule.Meeting.Request"),
		unicodeProperty(0x0037, "Réunion"),
		unicodeProperty(0x0c1a, "Sender"),
		unicodeProperty(0x0c1e, "EX"),
		unicodeProperty(0x0c1f, "/O=EXAMPLE/CN=SENDER"),
		unicodeProperty(0x007d, "From: person@example.test\r\n"),
		unicodeProperty(0x1035, "<meeting@example.test>"),
		unicodeProperty(0x1000, "Meeting body 日本語"),
		unicodeProperty(0x1013, "<p>Meeting body 日本語</p>"),
		{ID: 0x0039, Type: pstlib.PropertyTypeTime, Data: filetime},
	}
	entry := ExtractMessage(&pstlib.Message{Properties: &properties.Appointment{}, PropertyContext: &pstlib.PropertyContext{Properties: props}}, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	require.NoError(t, entry.BodyError)
	require.NoError(t, entry.DateError)
	assert.Equal(t, "Réunion", entry.Subject)
	assert.Equal(t, "From: person@example.test\r\n", entry.TransportHeaders)
	assert.Equal(t, "<meeting@example.test>", entry.MessageID)
	assert.Equal(t, "Meeting body 日本語", entry.BodyText)
	assert.Equal(t, "<p>Meeting body 日本語</p>", entry.BodyHTML)
	assert.Equal(t, time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), entry.SentAt)
	assert.Equal(t, &UnresolvedIdentity{Name: "Sender", AddressType: "EX", RawAddress: "/O=EXAMPLE/CN=SENDER"}, entry.UnresolvedSender)
}

func TestExtractMessage_AppointmentClasses(t *testing.T) {
	for _, class := range []string{"", "IPM.Appointment", "IPM.Schedule.Meeting", "IPM.Schedule.Meeting.Request.Custom"} {
		t.Run(class, func(t *testing.T) {
			msg := &pstlib.Message{Properties: &properties.Appointment{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{{ID: 0x001a, Type: pstlib.PropertyTypeString8, Data: []byte(class + "\x00")}}}}
			assert.Nil(t, ExtractMessage(msg, "Inbox"))
		})
	}
	msg := &pstlib.Message{Properties: &properties.Appointment{}, PropertyContext: &pstlib.PropertyContext{}}
	assert.Nil(t, ExtractMessage(msg, "Inbox"))
	msg.PropertyContext.Properties = []pstlib.Property{{ID: 0x001a, Type: pstlib.PropertyTypeString, Data: []byte{1}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.ErrorContains(t, entry.HeaderError, "odd-length Unicode MAPI string")
	msg.PropertyContext.Properties[0] = pstlib.Property{ID: 0x001a, Type: pstlib.PropertyTypeInteger32, Data: []byte{1, 0, 0, 0}}
	entry = ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.ErrorContains(t, entry.HeaderError, "unexpected type")
}

func TestExtractMessage_MeetingUnicodeBodyReadErrors(t *testing.T) {
	for _, id := range []uint16{0x1000, 0x1013} {
		t.Run(fmt.Sprintf("property_%04x", id), func(t *testing.T) {
			msg := &pstlib.Message{Properties: &properties.Appointment{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
				{ID: 0x001a, Type: pstlib.PropertyTypeString8, Data: []byte("IPM.Schedule.Meeting.Request\x00")},
				{ID: id, Type: pstlib.PropertyTypeString, Data: []byte{1}},
			}}}
			entry := ExtractMessage(msg, "Inbox")
			require.NotNil(t, entry)
			require.ErrorContains(t, entry.BodyError, "odd-length Unicode MAPI string")
		})
	}
}

func TestExtractMessage_ThreadingProperties(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	id, parent, refs := "<message@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"
	entry := ExtractMessage(&pstlib.Message{Identifier: 123, Properties: &properties.Message{InternetMessageId: &id, InReplyToId: &parent, InternetReferences: &refs}}, "Inbox")
	require.NotNil(entry)
	assert.Equal(id, entry.MessageID)
	assert.Equal(parent, entry.InReplyTo)
	assert.Equal(refs, entry.References)
}

func TestExtractMessage_BinaryHTML(t *testing.T) {
	codepage := make([]byte, 4)
	binary.LittleEndian.PutUint32(codepage, 1252)
	msg := &pstlib.Message{
		Identifier: 123,
		Properties: &properties.Message{},
		PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
			{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: []byte("<p>caf\xe9</p>\x00")},
			{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: codepage},
		}},
	}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	assert.Equal(t, "<p>café</p>", entry.BodyHTML)

	raw, err := BuildRFC5322(entry, nil)
	require.NoError(t, err)
	email, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Contains(t, email.Header.Get("Content-Type"), "text/html")
	body, err := io.ReadAll(quotedprintable.NewReader(email.Body))
	require.NoError(t, err)
	assert.Equal(t, "<p>café</p>\r\n", string(body))
}

func TestExtractMessage_UTF7Body(t *testing.T) {
	msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1000, Type: pstlib.PropertyTypeString8, Data: []byte("caf+AOk- and +- plus\x00")},
		{ID: 0x3ffd, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe8, 0xfd, 0, 0}},
	}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	assert.Equal(t, "café and + plus", entry.BodyText)

	msg.PropertyContext.Properties[0] = pstlib.Property{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: []byte("<p>caf+AOk-</p>\x00")}
	entry = ExtractMessage(msg, "Inbox")
	require.NoError(t, entry.BodyError)
	assert.Equal(t, "<p>café</p>", entry.BodyHTML)

	msg.PropertyContext.Properties[0].Data = []byte("<p>bad +?</p>")
	entry = ExtractMessage(msg, "Inbox")
	require.NoError(t, entry.BodyError)
	require.NotNil(t, entry.RawHTMLAttachment)
	assert.Equal(t, []byte("<p>bad +?</p>"), entry.RawHTMLAttachment.Content)
}

func TestExtractMessage_RTFOnlyIsReported(t *testing.T) {
	msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1009, Type: pstlib.PropertyTypeBinary, Data: []byte("rtf")},
	}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	assert.ErrorContains(t, entry.BodyError, "RTF-only")
	text := "normal body"
	msg.Properties = &properties.Message{Body: &text}
	assert.NoError(t, ExtractMessage(msg, "Inbox").BodyError)
}

func TestExtractMessage_UndecodableHTMLAndRTFPreservesBoth(t *testing.T) {
	htmlBytes := []byte("<meta charset=unknown-example><p>\xff</p>")
	rtfBytes := []byte{0x10, 0, 0, 0, 0x52, 0x54, 0x46, 0x00, 0xfe}
	msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: htmlBytes},
		{ID: 0x1009, Type: pstlib.PropertyTypeBinary, Data: rtfBytes},
	}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	require.NotNil(t, entry.RawHTMLAttachment)
	assert.Equal(t, htmlBytes, entry.RawHTMLAttachment.Content)
	require.NotNil(t, entry.RawRTFAttachment)
	assert.Equal(t, rtfBytes, entry.RawRTFAttachment.Content)
	assert.Equal(t, "original-pst-rtf-compressed.bin", entry.RawRTFAttachment.Filename)
	assert.Equal(t, "application/octet-stream", entry.RawRTFAttachment.MIMEType)

	for _, tc := range []struct {
		name string
		prop pstlib.Property
	}{
		{"unexpected type", pstlib.Property{ID: 0x1009, Type: pstlib.PropertyTypeInteger32, Data: []byte{1, 2, 3, 4}}},
		{"unreadable property", pstlib.Property{ID: 0x1009, Type: pstlib.PropertyTypeBinary}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
				{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: htmlBytes}, tc.prop,
			}}}
			entry := ExtractMessage(bad, "Inbox")
			require.NotNil(t, entry)
			require.ErrorContains(t, entry.BodyError, "RTF body")
			assert.Nil(t, entry.RawRTFAttachment)
		})
	}
}

func TestExtractMessage_BinaryHTMLErrors(t *testing.T) {
	tests := []struct {
		name  string
		props []pstlib.Property
	}{
		{"wrong property type", []pstlib.Property{{ID: 0x1013, Type: pstlib.PropertyTypeInteger32, Data: []byte{1, 2, 3, 4}}}},
		{"missing property data", []pstlib.Property{{ID: 0x1013, Type: pstlib.PropertyTypeBinary}}},
		{"invalid codepage property", []pstlib.Property{{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: []byte("hello")}, {ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{1}}}},
		{"invalid codepage with declared charset", []pstlib.Property{{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: []byte("<meta charset=utf-8>hello")}, {ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{1}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: tc.props}}
			entry := ExtractMessage(msg, "Inbox")
			require.NotNil(t, entry)
			assert.Error(t, entry.BodyError)
			assert.Nil(t, entry.RawHTMLAttachment)
		})
	}
}

func TestExtractMessage_BinaryHTMLDecodeRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     []byte
		codepage []byte
		want     string
	}{
		{"invalid UTF-8", []byte("<meta charset=utf-8><p>before \xff after</p>"), nil, "<meta charset=utf-8><p>before � after</p>"},
		{"valid replacement and invalid UTF-8", []byte("<meta charset=utf-8><p>� before \xff after</p>\x00"), nil, "<meta charset=utf-8><p>� before � after</p>"},
		{"unsupported codepage", []byte("<p>unknown \xff bytes</p>"), []byte{0x39, 0x30, 0, 0}, ""},
		{"unknown declared charset", []byte("<meta charset=not-a-real-encoding><p>unknown \xff bytes</p>"), nil, ""},
		{"unknown http equiv charset", []byte(`<meta http-equiv="content-type" content="text/html; charset=not-a-real-encoding"><p>test</p>`), nil, `<meta http-equiv="content-type" content="text/html; charset=not-a-real-encoding"><p>test</p>`},
		{"empty http equiv charset", []byte(`<meta http-equiv="content-type" content='text/html; charset=""'><p>test</p>`), nil, `<meta http-equiv="content-type" content='text/html; charset=""'><p>test</p>`},
		{"UTF8 MAPI overrides legacy meta", []byte("<meta charset=windows-1252><p>café</p>"), []byte{0xe9, 0xfd, 0, 0}, "<meta charset=windows-1252><p>café</p>"},
		{"conflicting charset invalid UTF8", []byte("<meta charset=utf-8><p>before \xff after</p>"), []byte{0xaf, 0x6f, 0, 0}, ""},
		{"UTF8 codepage invalid UTF8", []byte("<p>before \xff after</p>"), []byte{0xe9, 0xfd, 0, 0}, "<p>before � after</p>"},
		{"undeclared legacy bytes", []byte("<p>caf\xe9</p>"), nil, "<p>café</p>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			props := []pstlib.Property{{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: tc.data}}
			if tc.codepage != nil {
				props = append(props, pstlib.Property{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: tc.codepage})
			}
			entry := ExtractMessage(&pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: props}}, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.BodyError)
			assert.Equal(t, tc.want, entry.BodyHTML)
			require.NotNil(t, entry.RawHTMLAttachment)
			assert.Equal(t, tc.data, entry.RawHTMLAttachment.Content)
			assert.Equal(t, "original-pst-html.bin", entry.RawHTMLAttachment.Filename)
			assert.Equal(t, "application/octet-stream", entry.RawHTMLAttachment.MIMEType)
		})
	}
}

func TestExtractMessage_BinaryHTMLMetaCharset(t *testing.T) {
	msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: []byte("<meta charset=utf-8><p>日本語</p>")},
	}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	assert.Equal(t, "<meta charset=utf-8><p>日本語</p>", entry.BodyHTML)
}

func TestExtractMessage_DeclaredHTMLCharsetOverridesCodepage(t *testing.T) {
	for _, tc := range []struct {
		name string
		html []byte
		want string
	}{
		{"meta", []byte("<meta charset=utf-8><p>café</p>"), "<meta charset=utf-8><p>café</p>"},
		{"http-equiv", []byte(`<meta http-equiv="content-type" content="text/html; charset=utf-8"><p>café</p>`), `<meta http-equiv="content-type" content="text/html; charset=utf-8"><p>café</p>`},
		{"bom", append([]byte{0xef, 0xbb, 0xbf}, []byte("<p>café</p>")...), "<p>café</p>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
				{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: tc.html},
				{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe4, 0x04, 0, 0}}, // 1252
			}}}
			entry := ExtractMessage(msg, "Inbox")
			require.NoError(t, entry.BodyError)
			assert.Equal(t, tc.want, entry.BodyHTML)
		})
	}
}

func TestExtractMessage_String8Body(t *testing.T) {
	msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1000, Type: pstlib.PropertyTypeString8, Data: []byte("caf\xe9\x00")},
		{ID: 0x3ffd, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe4, 0x04, 0, 0}}, // 1252
	}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NoError(t, entry.BodyError)
	assert.Equal(t, "café", entry.BodyText)

	// A missing codepage uses the documented Windows-1252 fallback.
	msg.PropertyContext.Properties = msg.PropertyContext.Properties[:1]
	entry = ExtractMessage(msg, "Inbox")
	require.NoError(t, entry.BodyError)
	assert.Equal(t, "café", entry.BodyText)

	// An explicitly unsupported codepage must not silently yield an empty body.
	msg.PropertyContext.Properties = append(msg.PropertyContext.Properties, pstlib.Property{
		ID: 0x3ffd, Type: pstlib.PropertyTypeInteger32, Data: []byte{0x39, 0x30, 0, 0},
	})
	entry = ExtractMessage(msg, "Inbox")
	assert.ErrorContains(t, entry.BodyError, "unsupported body codepage")
}

func TestExtractMessage_BinaryHTMLUTF16(t *testing.T) {
	msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: []byte{'<', 0, 'p', 0, '>', 0, 0xe9, 0, '<', 0, '/', 0, 'p', 0, '>', 0, 0, 0}},
		{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xb0, 0x04, 0, 0}},
	}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	assert.Equal(t, "<p>é</p>", entry.BodyHTML)
}

func TestExtractMessage_RealBinaryHTML(t *testing.T) {
	f, err := Open(filepath.Join("testdata", "support.pst"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, f.Close()) })
	found := 0
	require.NoError(t, f.WalkFolders(func(folder FolderEntry, rawFolder *pstlib.Folder) error {
		iter, err := rawFolder.GetMessageIterator()
		if err != nil {
			return nil //nolint:nilerr // empty folder
		}
		for iter.Next() {
			msg := iter.Value()
			prop, err := msg.PropertyContext.GetPropertyByID(0x1013)
			if err != nil || prop.Type != pstlib.PropertyTypeBinary {
				continue
			}
			entry := ExtractMessage(msg, folder.Path)
			if entry == nil {
				continue
			}
			require.NoError(t, entry.BodyError)
			assert.NotEmpty(t, entry.BodyHTML)
			found++
		}
		return iter.Err()
	}))
	assert.Positive(t, found, "fixture must contain a binary HTML body")
}

func TestExtractMessage_RealRecipientTable(t *testing.T) {
	f, err := Open(filepath.Join("testdata", "support.pst"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, f.Close()) })
	totalMessages, totalRecipients := 0, 0
	require.NoError(t, f.WalkFolders(func(folder FolderEntry, rawFolder *pstlib.Folder) error {
		if folder.MsgCount == 0 {
			return nil
		}
		iter, err := rawFolder.GetMessageIterator()
		if err != nil {
			return err
		}
		for iter.Next() {
			entry := ExtractMessage(iter.Value(), folder.Path)
			if entry == nil {
				continue
			}
			require.NoError(t, entry.HeaderError)
			totalMessages++
			totalRecipients += len(entry.Recipients)
		}
		return iter.Err()
	}))
	assert.Equal(t, 17, totalMessages)
	assert.Equal(t, 1073, totalRecipients)
}

func TestExtractMessage_String8Headers(t *testing.T) {
	msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x0037, Type: pstlib.PropertyTypeString8, Data: []byte("caf\xe9 subject\x00")},
		{ID: 0x0c1a, Type: pstlib.PropertyTypeString8, Data: []byte("Jos\xe9\x00")},
		{ID: 0x0c1f, Type: pstlib.PropertyTypeString8, Data: []byte("sender@example.test\x00")},
		{ID: 0x007d, Type: pstlib.PropertyTypeString8, Data: []byte("To: receiver@example.test\r\n\x00")},
		{ID: 0x3ffd, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe4, 0x04, 0, 0}},
	}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	assert.Equal(t, "café subject", entry.Subject)
	assert.Equal(t, "José", entry.SenderName)
	assert.Equal(t, "sender@example.test", entry.SenderEmail)
	assert.Equal(t, "To: receiver@example.test\r\n", entry.TransportHeaders)
}

func TestExtractMessage_ExchangeIdentities(t *testing.T) {
	senderName, senderDN, senderType := "Sender Name", "/O=EXAMPLE/CN=SENDER", "EX"
	authorName, authorDN, authorType := "Author Name", "/O=EXAMPLE/CN=AUTHOR", "EX"
	tests := []struct {
		name                     string
		props                    *properties.Message
		additional               []pstlib.Property
		wantSender, wantAuthor   *UnresolvedIdentity
		senderEmail, authorEmail string
	}{
		{name: "sender only", props: &properties.Message{SenderName: &senderName, SenderEmailAddress: &senderDN, SenderAddressType: &senderType}, wantSender: &UnresolvedIdentity{Name: senderName, AddressType: senderType, RawAddress: senderDN}},
		{name: "represented author only", props: &properties.Message{SentRepresentingName: &authorName, SentRepresentingEmailAddress: &authorDN, SentRepresentingAddressType: &authorType}, wantAuthor: &UnresolvedIdentity{Name: authorName, AddressType: authorType, RawAddress: authorDN}},
		{name: "both", props: &properties.Message{SenderName: &senderName, SenderEmailAddress: &senderDN, SenderAddressType: &senderType, SentRepresentingName: &authorName, SentRepresentingEmailAddress: &authorDN, SentRepresentingAddressType: &authorType}, wantSender: &UnresolvedIdentity{Name: senderName, AddressType: senderType, RawAddress: senderDN}, wantAuthor: &UnresolvedIdentity{Name: authorName, AddressType: authorType, RawAddress: authorDN}},
		{name: "SMTP overrides Exchange", props: &properties.Message{SenderName: &senderName, SenderEmailAddress: &senderDN, SenderAddressType: &senderType, SentRepresentingName: &authorName, SentRepresentingEmailAddress: &authorDN, SentRepresentingAddressType: &authorType}, additional: []pstlib.Property{{ID: 0x5d01, Type: pstlib.PropertyTypeString8, Data: []byte("sender@example.test\x00")}, {ID: 0x5d02, Type: pstlib.PropertyTypeString8, Data: []byte("author@example.test\x00")}}, senderEmail: "sender@example.test", authorEmail: "author@example.test"},
		{name: "invalid SMTP does not hide Exchange", props: &properties.Message{SenderName: &senderName, SenderEmailAddress: &senderDN, SenderAddressType: &senderType}, additional: []pstlib.Property{{ID: 0x5d01, Type: pstlib.PropertyTypeString8, Data: []byte("not an address\x00")}}, wantSender: &UnresolvedIdentity{Name: senderName, AddressType: senderType, RawAddress: senderDN, RawSMTP: "not an address"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := ExtractMessage(&pstlib.Message{Properties: tc.props, PropertyContext: &pstlib.PropertyContext{Properties: tc.additional}}, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.HeaderError)
			assert.Equal(t, tc.wantSender, entry.UnresolvedSender)
			assert.Equal(t, tc.wantAuthor, entry.UnresolvedAuthor)
			assert.Equal(t, tc.senderEmail, entry.SenderEmail)
			assert.Equal(t, tc.authorEmail, entry.AuthorEmail)
		})
	}
}

func TestExtractMessage_ExchangeIdentityReadErrors(t *testing.T) {
	for _, id := range []uint16{0x0c1a, 0x0c1e, 0x0c1f, 0x5d01, 0x0042, 0x0064, 0x0065, 0x5d02} {
		t.Run(fmt.Sprintf("property_%04x", id), func(t *testing.T) {
			msg := &pstlib.Message{Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{{ID: id, Type: pstlib.PropertyTypeString, Data: []byte{1}}}}}
			entry := ExtractMessage(msg, "Inbox")
			require.NotNil(t, entry)
			assert.ErrorContains(t, entry.HeaderError, "odd-length Unicode MAPI string")
		})
	}
}

func TestMakeMessageRecipient(t *testing.T) {
	r, err := makeMessageRecipient(2, "Alice", "SMTP", "alice@example.test", "")
	require.NoError(t, err)
	assert.Equal(t, &MessageRecipient{Name: "Alice", Email: "alice@example.test", Role: 2}, r)
	r, err = makeMessageRecipient(3, "Bob", "EX", "/O=EXAMPLE/CN=BOB", "bob@example.test")
	require.NoError(t, err)
	assert.Equal(t, &MessageRecipient{Name: "Bob", Email: "bob@example.test", Role: 3}, r)
	r, err = makeMessageRecipient(1, "Bob", "EX", "/O=EXAMPLE/CN=BOB", "")
	require.NoError(t, err)
	assert.Equal(t, &MessageRecipient{Name: "Bob", Role: 1, Unresolved: true, AddressType: "EX", RawAddress: "/O=EXAMPLE/CN=BOB"}, r)
	r, err = makeMessageRecipient(0, "", "", "", "")
	require.NoError(t, err)
	assert.Nil(t, r)
}
