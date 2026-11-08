package pst

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cention-sany/utf7"
	charsets "github.com/emersion/go-message/charset"
	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/rotisserie/eris"
	"golang.org/x/net/html"
	htmlcharset "golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/unicode"
)

func init() {
	// Register extended charsets so go-pst can decode non-UTF-8 encoded PST files.
	pstlib.ExtendCharsets(func(name string, enc encoding.Encoding) {
		charsets.RegisterEncoding(name, enc)
	})
}

// messageDate reads the original FILETIME; go-pst's decoded property is Unix
// nanoseconds and cannot represent all FILETIME values, including raw zero.
func messageDate(msg *pstlib.Message, id uint16, decoded *int64) (time.Time, error) {
	if msg.PropertyContext == nil {
		if decoded == nil {
			return time.Time{}, nil
		}
		return time.Unix(0, *decoded).UTC(), nil
	}
	prop, err := msg.PropertyContext.GetPropertyByID(id)
	if errors.Is(err, pstlib.ErrPropertyNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if prop.Type != pstlib.PropertyTypeTime {
		return time.Time{}, fmt.Errorf("property %#x has unexpected type %v", id, prop.Type)
	}
	data := prop.Data
	if len(data) == 0 {
		reader, err := msg.PropertyContext.GetPropertyReader(id, msg.LocalDescriptors)
		if err != nil {
			return time.Time{}, err
		}
		if reader.HeapOnNodeReader == nil || reader.Size() != 8 {
			return time.Time{}, fmt.Errorf("property %#x has invalid date data", id)
		}
		data = make([]byte, 8)
		if n, err := reader.ReadAt(data, 0); err != nil || n != len(data) {
			return time.Time{}, fmt.Errorf("read date property %#x: read %d bytes: %v", id, n, err)
		}
	}
	if len(data) != 8 {
		return time.Time{}, fmt.Errorf("property %#x has %d date bytes, expected 8", id, len(data))
	}
	ft := binary.LittleEndian.Uint64(data)
	if ft == 0 {
		return time.Time{}, nil
	}
	const epochDiff int64 = 11644473600
	return time.Unix(int64(ft/10_000_000)-epochDiff, int64(ft%10_000_000)*100).UTC(), nil
}

const maxPSTBodyBytes = 128 << 20

var errUnsupportedBodyCodepage = errors.New("unsupported body codepage")

// bodyPropertyBytes reads a variable-length property with a bounded allocation.
func bodyPropertyBytes(msg *pstlib.Message, prop pstlib.Property) ([]byte, error) {
	data := prop.Data
	if len(data) != 0 {
		if len(data) > maxPSTBodyBytes {
			return nil, fmt.Errorf("body property %#x exceeds %d bytes", prop.ID, maxPSTBodyBytes)
		}
		return data, nil
	}
	r, err := msg.PropertyContext.GetPropertyReader(prop.ID, msg.LocalDescriptors)
	if err != nil {
		return nil, fmt.Errorf("open body property %#x: %w", prop.ID, err)
	}
	if r.HeapOnNodeReader == nil || r.Size() < 0 || r.Size() > maxPSTBodyBytes {
		return nil, fmt.Errorf("body property %#x has invalid or oversized data", prop.ID)
	}
	data = make([]byte, int(r.Size()))
	if len(data) > 0 {
		if n, err := r.ReadAt(data, 0); err != nil || n != len(data) {
			return nil, fmt.Errorf("read body property %#x: read %d bytes: %v", prop.ID, n, err)
		}
	}
	return data, nil
}

// messageCodepage returns the first present supported MAPI codepage.
func messageCodepage(msg *pstlib.Message, ids ...uint16) (string, error) {
	for _, id := range ids {
		cp, err := msg.PropertyContext.GetPropertyByID(id)
		if errors.Is(err, pstlib.ErrPropertyNotFound) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("find body codepage: %w", err)
		}
		if cp.Type != pstlib.PropertyTypeInteger32 || len(cp.Data) != 4 {
			return "", fmt.Errorf("body codepage property %#x is invalid", id)
		}
		value := binary.LittleEndian.Uint32(cp.Data)
		if value == 0 {
			continue
		}
		name := pstlib.CodePageIdentifierToEncoding[int(value)]
		switch value {
		case 1200:
			name = "utf-16le"
		case 1201:
			name = "utf-16be"
		case 932:
			name = "shift_jis"
		}
		if enc, _ := htmlcharset.Lookup(name); enc == nil && value != 65000 {
			return "", fmt.Errorf("%w %d", errUnsupportedBodyCodepage, value)
		}
		return name, nil
	}
	return "", nil
}

// htmlDeclaredCharset recognizes a BOM or a meta charset in the part of
// the document inspected by the HTML charset sniffer. Unknown declarations
// are distinct from absence: interpreting them as a guessed charset can alter
// the original text without any indication that decoding was uncertain.
func htmlDeclaredCharset(data []byte) (name string, declared bool) {
	for _, bom := range []struct {
		bytes []byte
		name  string
	}{{[]byte{0xef, 0xbb, 0xbf}, "utf-8"}, {[]byte{0xff, 0xfe}, "utf-16le"}, {[]byte{0xfe, 0xff}, "utf-16be"}} {
		if bytes.HasPrefix(data, bom.bytes) {
			return bom.name, true
		}
	}
	tokenizer := html.NewTokenizer(bytes.NewReader(data[:min(len(data), 1024)]))
	for {
		typeOfToken := tokenizer.Next()
		if typeOfToken == html.ErrorToken {
			return "", false
		}
		if typeOfToken != html.StartTagToken && typeOfToken != html.SelfClosingTagToken {
			continue
		}
		tag := tokenizer.Token()
		if tag.Data != "meta" {
			continue
		}
		var content, httpEquiv string
		for _, attr := range tag.Attr {
			switch attr.Key {
			case "charset":
				_, name := htmlcharset.Lookup(attr.Val)
				return name, true
			case "http-equiv":
				httpEquiv = attr.Val
			case "content":
				content = attr.Val
			}
		}
		if strings.EqualFold(httpEquiv, "content-type") {
			_, params, err := mime.ParseMediaType(content)
			if charset, ok := params["charset"]; err == nil && ok {
				_, name := htmlcharset.Lookup(charset)
				return name, true
			}
			if err != nil && strings.Contains(strings.ToLower(content), "charset") {
				return "", true
			}
		}
	}
}

// messageHTML retrieves PidTagHtml, which is normally a binary property and is
// therefore not populated by go-pst's generated Message properties. HTML can
// declare a charset itself or use the message's Internet code page.
func messageHTML(msg *pstlib.Message, decoded string) (string, *AttachmentEntry, error) {
	if decoded != "" || msg.PropertyContext == nil {
		return decoded, nil, nil
	}
	prop, err := msg.PropertyContext.GetPropertyByID(0x1013)
	if errors.Is(err, pstlib.ErrPropertyNotFound) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("find HTML body: %w", err)
	}
	if prop.Type == pstlib.PropertyTypeString {
		// The generated Unicode property is already populated, including an
		// intentionally empty string.
		return decoded, nil, nil
	}
	if prop.Type != pstlib.PropertyTypeBinary && prop.Type != pstlib.PropertyTypeString8 {
		return "", nil, fmt.Errorf("HTML body has unexpected property type %v", prop.Type)
	}

	data, err := bodyPropertyBytes(msg, prop)
	if err != nil {
		return "", nil, err
	}
	if len(data) == 0 {
		return "", nil, nil
	}
	preserve := func() *AttachmentEntry {
		return &AttachmentEntry{Filename: "original-pst-html.bin", MIMEType: "application/octet-stream", Content: data}
	}

	declaredName, declared := htmlDeclaredCharset(data)
	name, codepageErr := messageCodepage(msg, 0x3fde, 0x3ffd) // Internet, then message codepage
	if codepageErr != nil && !errors.Is(codepageErr, errUnsupportedBodyCodepage) {
		return "", nil, codepageErr
	}
	canonicalName := name
	if name != "" && !strings.EqualFold(name, "utf-7") {
		_, canonicalName = htmlcharset.Lookup(name)
	}
	conflict := declared && (codepageErr != nil || canonicalName != "" && canonicalName != declaredName)
	// An explicit UTF-8 codepage backed by valid UTF-8 bytes gives an
	// unambiguous decoding even when the HTML declaration says otherwise.
	if declared && declaredName != "utf-8" && canonicalName == "utf-8" && utf8.Valid(data) {
		return normalizeHTML(string(data)), preserve(), nil
	}
	if (declared && declaredName == "") || (!declared && codepageErr != nil) {
		return asciiHTML(data), preserve(), nil //nolint:nilerr // Unsupported encodings are preserved as attachments.
	}
	if conflict && (declaredName != "utf-8" || !utf8.Valid(data)) {
		// Conflicting declarations do not identify a trustworthy encoding.
		return asciiHTML(data), preserve(), nil
	}

	contentType := "text/html"
	var r io.Reader
	if !declared && name != "" {
		if strings.EqualFold(name, "utf-7") {
			r = utf7.UTF7.NewDecoder().Reader(bytes.NewReader(bytes.TrimRight(data, "\x00")))
		} else {
			contentType += "; charset=" + name
		}
	}
	if r == nil {
		r, err = htmlcharset.NewReader(bytes.NewReader(data), contentType)
		if err != nil {
			return "", preserve(), nil //nolint:nilerr // Preserve bytes when a decoder cannot be selected.
		}
	}
	out, err := io.ReadAll(io.LimitReader(r, maxPSTBodyBytes+1))
	if len(out) > maxPSTBodyBytes {
		return "", nil, fmt.Errorf("decoded HTML body exceeds %d bytes", maxPSTBodyBytes)
	}
	if err != nil || !utf8.Valid(out) || bytes.Count(out, []byte("\ufffd")) > bytes.Count(data, []byte("\ufffd")) {
		// Decoders can return a readable prefix with an error or replacement
		// characters. Keep that best-effort text and the exact original bytes.
		return normalizeHTML(strings.ToValidUTF8(string(out), "\ufffd")), preserve(), nil //nolint:nilerr // Decoder errors retain original bytes.
	}
	if conflict || (!declared && name == "" && !utf8.Valid(data)) {
		return normalizeHTML(string(out)), preserve(), nil
	}
	return normalizeHTML(string(out)), nil, nil
}

// When a charset is unknown, literal ASCII is readable without assigning a
// meaning to any non-ASCII bytes. Never strip controls from an unknown byte
// stream and claim that the remainder is the original text.
func asciiHTML(data []byte) string {
	for _, b := range data {
		if b != '\t' && b != '\n' && b != '\r' && (b < 0x20 || b > 0x7e) {
			return ""
		}
	}
	return string(data)
}

func normalizeHTML(value string) string {
	return strings.TrimPrefix(strings.TrimRight(value, "\x00"), "\ufeff")
}

// messageText recovers String8 PidTagBody, which go-pst does not populate in
// its Unicode-only generated Body field. If the archive lacks a codepage,
// Windows-1252 is the fallback used for older Western Outlook data.
func messageText(msg *pstlib.Message, decoded string) (string, error) {
	if msg.PropertyContext == nil {
		return decoded, nil
	}
	prop, err := msg.PropertyContext.GetPropertyByID(0x1000)
	if errors.Is(err, pstlib.ErrPropertyNotFound) {
		return decoded, nil
	}
	if err != nil {
		return "", fmt.Errorf("find text body: %w", err)
	}
	if prop.Type == pstlib.PropertyTypeString {
		return decoded, nil
	}
	if prop.Type != pstlib.PropertyTypeString8 {
		return "", fmt.Errorf("text body has unexpected property type %v", prop.Type)
	}
	data, err := bodyPropertyBytes(msg, prop)
	if err != nil {
		return "", err
	}
	name, err := messageCodepage(msg, 0x3ffd, 0x3fde) // message, then Internet codepage
	if err != nil {
		return "", err
	}
	if name == "" {
		name = "windows-1252"
	}
	var enc encoding.Encoding
	if strings.EqualFold(name, "utf-7") {
		enc = utf7.UTF7
		data = bytes.TrimRight(data, "\x00")
	} else {
		enc, _ = htmlcharset.Lookup(name)
	}
	out, err := io.ReadAll(io.LimitReader(enc.NewDecoder().Reader(bytes.NewReader(data)), maxPSTBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("decode text body: %w", err)
	}
	if len(out) > maxPSTBodyBytes {
		return "", fmt.Errorf("decoded text body exceeds %d bytes", maxPSTBodyBytes)
	}
	if !utf8.Valid(out) || bytes.Contains(out, []byte("\ufffd")) && !bytes.Contains(data, []byte("\ufffd")) {
		return "", errors.New("text body contains undecodable characters")
	}
	return strings.TrimRight(string(out), "\x00"), nil
}

// propertyString reads both Unicode and String8 MAPI strings. The generated
// go-pst fields only contain Unicode values and its String8 default is UTF-8.
func propertyString(msg *pstlib.Message, id uint16, fallback string) (string, error) {
	if msg.PropertyContext == nil {
		return fallback, nil
	}
	prop, err := msg.PropertyContext.GetPropertyByID(id)
	if errors.Is(err, pstlib.ErrPropertyNotFound) {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	if prop.Type == pstlib.PropertyTypeString && fallback != "" {
		return fallback, nil
	}
	if prop.Type != pstlib.PropertyTypeString && prop.Type != pstlib.PropertyTypeString8 {
		return "", fmt.Errorf("string property %#x has unexpected type %v", id, prop.Type)
	}
	if prop.HNID == 0 && len(prop.Data) == 0 {
		return "", nil
	}
	data, err := bodyPropertyBytes(msg, prop)
	if err != nil {
		return "", err
	}
	return decodeMAPIString(msg, prop.Type, data)
}

func decodeMAPIString(msg *pstlib.Message, typ pstlib.PropertyType, data []byte) (string, error) {
	var dec *encoding.Decoder
	switch typ {
	case pstlib.PropertyTypeString:
		if len(data)%2 != 0 {
			return "", errors.New("odd-length Unicode MAPI string")
		}
		dec = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	case pstlib.PropertyTypeString8:
		name, err := messageCodepage(msg, 0x3ffd, 0x3fde)
		if err != nil {
			return "", err
		}
		if name == "" {
			name = "windows-1252"
		}
		var enc encoding.Encoding
		if strings.EqualFold(name, "utf-7") {
			enc = utf7.UTF7
			data = bytes.TrimRight(data, "\x00")
		} else {
			enc, _ = htmlcharset.Lookup(name)
		}
		dec = enc.NewDecoder()
	default:
		return "", fmt.Errorf("unexpected MAPI string type %v", typ)
	}
	out, err := io.ReadAll(io.LimitReader(dec.Reader(bytes.NewReader(data)), maxPSTBodyBytes+1))
	if err != nil || len(out) > maxPSTBodyBytes {
		return "", fmt.Errorf("decode MAPI string: invalid or oversized data: %v", err)
	}
	if !utf8.Valid(out) || bytes.Contains(out, []byte("\ufffd")) && !bytes.Contains(data, []byte("\ufffd")) {
		return "", errors.New("MAPI string contains undecodable characters")
	}
	return strings.TrimRight(string(out), "\x00"), nil
}

// MessageRecipient retains the MAPI role and address independently of the
// often-incomplete transport headers.
type MessageRecipient struct {
	Name, Email                      string
	Role                             int32 // 1 = To, 2 = Cc, 3 = Bcc
	Unresolved                       bool
	AddressType, RawAddress, RawSMTP string
}

// messageRecipients opens NID_RECIP_TABLE (0x692) from the message's local
// descriptors using the same primitives go-pst uses for attachment tables.
func messageRecipients(msg *pstlib.Message) ([]MessageRecipient, error) {
	if msg.File == nil || msg.PropertyContext == nil {
		return nil, nil // synthetic messages or messages without a file
	}
	desc, err := pstlib.FindLocalDescriptor(0x692, msg.LocalDescriptors)
	if errors.Is(err, pstlib.ErrLocalDescriptorNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find recipient table: %w", err)
	}
	heap, err := msg.File.GetHeapOnNodeFromLocalDescriptor(desc)
	if err != nil {
		return nil, fmt.Errorf("open recipient table: %w", err)
	}
	locals, err := msg.File.GetLocalDescriptorsFromIdentifier(desc.LocalDescriptorsIdentifier)
	if err != nil {
		return nil, fmt.Errorf("read recipient table descriptors: %w", err)
	}
	table, err := msg.File.GetTableContext(heap, locals, 0x0c15, 0x3001, 0x3002, 0x3003, 0x39fe)
	if errors.Is(err, pstlib.ErrTableContextNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read recipient table: %w", err)
	}
	var recipients []MessageRecipient
	for _, row := range table.Properties {
		var role int32
		var name, addrType, email, smtp string
		for _, prop := range row {
			if prop.ID != 0x0c15 && prop.HNID == 0 && len(prop.Data) == 0 {
				continue // an explicitly empty optional string
			}
			r, err := table.GetPropertyReader(prop, locals...)
			if err != nil {
				return nil, fmt.Errorf("open recipient property %#x: %w", prop.ID, err)
			}
			if prop.ID == 0x0c15 {
				role, err = r.GetInteger32()
			} else {
				var data []byte
				if len(prop.Data) > 0 {
					data = prop.Data
				} else if r.HeapOnNodeReader == nil || r.Size() < 0 || r.Size() > maxPSTBodyBytes {
					err = errors.New("invalid or oversized recipient string")
				} else {
					data = make([]byte, int(r.Size()))
					if len(data) > 0 {
						var n int
						n, err = r.ReadAt(data, 0)
						if err == nil && n != len(data) {
							err = io.ErrUnexpectedEOF
						}
					}
				}
				var value string
				if err == nil {
					value, err = decodeMAPIString(msg, prop.Type, data)
				}
				switch prop.ID {
				case 0x3001:
					name = value
				case 0x3002:
					addrType = value
				case 0x3003:
					email = value
				case 0x39fe:
					smtp = value
				}
			}
			if err != nil {
				return nil, fmt.Errorf("read recipient property %#x: %w", prop.ID, err)
			}
		}
		recipient, err := makeMessageRecipient(role, name, addrType, email, smtp)
		if err != nil {
			return nil, err
		}
		if recipient != nil {
			recipients = append(recipients, *recipient)
		}
	}
	return recipients, nil
}

func makeMessageRecipient(role int32, name, addrType, email, smtp string) (*MessageRecipient, error) {
	if role < 1 || role > 3 {
		return nil, nil
	}
	rawSMTP := smtp
	if smtp == "" && (addrType == "" || strings.EqualFold(addrType, "SMTP")) {
		smtp = email
	}
	parsed, err := mail.ParseAddress(strings.TrimSpace(smtp))
	if err != nil || !strings.Contains(parsed.Address, "@") {
		return &MessageRecipient{Name: name, Role: role, Unresolved: true, AddressType: addrType, RawAddress: email, RawSMTP: rawSMTP}, nil
	}
	return &MessageRecipient{Name: name, Email: parsed.Address, Role: role}, nil
}

// File wraps a PST file for reading.
type File struct {
	pstFile *pstlib.File
	closer  io.Closer
}

// Open opens a PST file at path for reading.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	pstFile, err := pstlib.New(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("parse pst: %w", err)
	}
	return &File{pstFile: pstFile, closer: f}, nil
}

// Close releases all resources held by the File.
func (f *File) Close() error {
	f.pstFile.Cleanup()
	return f.closer.Close()
}

// FolderEntry holds metadata about a PST folder.
type FolderEntry struct {
	Name     string
	Path     string // Full slash-separated path, e.g. "Personal Folders/Inbox/Archive"
	MsgCount int32
}

// UnresolvedIdentity retains an Exchange identity for which no usable SMTP
// address was available. Its values are preserved as data, not email headers.
type UnresolvedIdentity struct {
	Name, AddressType, RawAddress, RawSMTP string
}

// MessageEntry holds the extracted email properties from a PST message.
type MessageEntry struct {
	// EntryID is the PST node identifier, used as part of the dedup key.
	EntryID string

	// FolderPath is the slash-separated folder path this message came from.
	FolderPath string

	// Subject is the message subject.
	Subject string

	// TransportMessageHeaders contains the original RFC 5322 headers as stored
	// by the PST, available for most internet-delivered messages. Empty for
	// drafts or messages sent via Exchange-only paths.
	TransportHeaders string

	// Body content.
	BodyText          string
	BodyHTML          string
	BodyError         error            // unreadable or unsupported body, if any
	RawHTMLAttachment *AttachmentEntry // original bytes when HTML cannot be decoded losslessly

	// Sender fields.
	SenderName        string
	SenderEmail       string
	SenderAddressType string // "SMTP" or "EX" (Exchange DN)
	AuthorName        string // Sent-representing identity, when present
	AuthorEmail       string
	UnresolvedSender  *UnresolvedIdentity
	UnresolvedAuthor  *UnresolvedIdentity
	Recipients        []MessageRecipient
	HeaderError       error // unreadable MAPI header data

	// DisplayTo/Cc/Bcc are semicolon-separated display names from MAPI.
	// Email addresses must be parsed from TransportHeaders when available.
	DisplayTo  string
	DisplayCc  string
	DisplayBcc string

	// Threading headers.
	MessageID  string
	InReplyTo  string
	References string

	// Timestamps.
	SentAt       time.Time // ClientSubmitTime
	ReceivedAt   time.Time // MessageDeliveryTime
	CreationTime time.Time // PidTagCreationTime
	DateError    error     // unreadable timestamp properties, if any
}

// FallbackDate picks a plausible PST date when no reliable message date exists.
func (entry *MessageEntry) FallbackDate(now time.Time) time.Time {
	earliest := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	latest := now.AddDate(0, 0, 30)
	for _, date := range []time.Time{entry.SentAt, entry.ReceivedAt, entry.CreationTime} {
		if !date.IsZero() && !date.Before(earliest) && !date.After(latest) {
			return date
		}
	}
	return time.Time{}
}

// AttachmentEntry holds metadata and content for a PST attachment.
type AttachmentEntry struct {
	Filename  string
	MIMEType  string
	ContentID string
	Size      int32
	Content   []byte
}

// WalkFolderFunc is called for each folder during WalkFolders. The raw pstlib
// folder is provided so callers can iterate messages.
type WalkFolderFunc func(entry FolderEntry, folder *pstlib.Folder) error

// WalkFolders walks all folders in the PST file recursively, building
// slash-separated folder paths. Search folders are skipped automatically.
// Returns an error if a folder cannot be read or fn returns an error.
func (f *File) WalkFolders(fn WalkFolderFunc) error {
	rootFolder, err := f.pstFile.GetRootFolder()
	if err != nil {
		return fmt.Errorf("get root folder: %w", err)
	}
	return walkFoldersRecursive(&rootFolder, "", fn)
}

func walkFoldersRecursive(folder *pstlib.Folder, parentPath string, fn WalkFolderFunc) error {
	// Search folders are saved queries over messages stored in real folders.
	// go-pst refuses to iterate them even when their stored count is nonzero.
	if folder.Identifier.GetType() == pstlib.IdentifierTypeSearchFolder {
		return nil
	}

	path := folder.Name
	if parentPath != "" {
		path = parentPath + "/" + folder.Name
	}

	entry := FolderEntry{
		Name:     folder.Name,
		Path:     path,
		MsgCount: folder.MessageCount,
	}

	if err := fn(entry, folder); err != nil {
		return err
	}

	if !folder.HasSubFolders {
		return nil
	}

	subFolders, err := folder.GetSubFolders()
	if err != nil {
		return fmt.Errorf("read subfolders of %q: %w", path, err)
	}
	for i := range subFolders {
		if err := walkFoldersRecursive(&subFolders[i], path, fn); err != nil {
			return err
		}
	}
	return nil
}

// ExtractMessage extracts email properties from a pstlib.Message.
// Returns nil if the message is not an email (e.g. calendar, contact, task).
func ExtractMessage(msg *pstlib.Message, folderPath string) *MessageEntry {
	props, ok := msg.Properties.(*properties.Message)
	if !ok {
		return nil
	}

	var headerErrors []error
	readString := func(id uint16, fallback string) string {
		value, err := propertyString(msg, id, fallback)
		if err != nil {
			headerErrors = append(headerErrors, fmt.Errorf("read property %#x: %w", id, err))
		}
		return value
	}
	subject := readString(0x0037, props.GetSubject())
	if subject == "" {
		subject = readString(0x0e1d, props.GetNormalizedSubject())
	}
	if subject == "" {
		subject = props.GetInternetSubject()
	}

	senderName := readString(0x0c1a, props.GetSenderName())
	senderType := readString(0x0c1e, props.GetSenderAddressType())
	senderEmail := readString(0x0c1f, props.GetSenderEmailAddress())
	senderSMTP := readString(0x5d01, "")
	authorName := readString(0x0042, props.GetSentRepresentingName())
	authorType := readString(0x0064, props.GetSentRepresentingAddressType())
	authorEmail := readString(0x0065, props.GetSentRepresentingEmailAddress())
	authorSMTP := readString(0x5d02, "")
	senderEmail, unresolvedSender := resolveMessageIdentity(senderName, senderType, senderEmail, senderSMTP)
	authorEmail, unresolvedAuthor := resolveMessageIdentity(authorName, authorType, authorEmail, authorSMTP)
	transportHeaders := readString(0x007d, props.GetTransportMessageHeaders())
	displayTo := readString(0x0e04, props.GetDisplayTo())
	displayCc := readString(0x0e03, props.GetDisplayCc())
	displayBcc := readString(0x0e02, props.GetDisplayBcc())
	messageID := readString(0x1035, props.GetInternetMessageId())
	inReplyTo := readString(0x1042, props.GetInReplyToId())
	references := readString(0x1039, props.GetInternetReferences())
	recipients, recipientErr := messageRecipients(msg)
	if recipientErr != nil {
		headerErrors = append(headerErrors, recipientErr)
	}

	sentAt, sentErr := messageDate(msg, 0x0039, props.ClientSubmitTime)
	receivedAt, receivedErr := messageDate(msg, 0x0e06, props.MessageDeliveryTime)
	createdAt, createdErr := messageDate(msg, 0x3007, props.CreationTime)
	bodyHTML, rawHTML, htmlErr := messageHTML(msg, props.GetBodyHtml())
	bodyText, textErr := messageText(msg, props.GetBody())
	bodyErr := errors.Join(htmlErr, textErr)
	if bodyErr == nil && bodyText == "" && bodyHTML == "" && msg.PropertyContext != nil {
		if _, err := msg.PropertyContext.GetPropertyByID(0x1009); err == nil {
			bodyErr = errors.New("RTF-only body requires conversion")
		} else if !errors.Is(err, pstlib.ErrPropertyNotFound) {
			bodyErr = fmt.Errorf("find RTF body: %w", err)
		}
	}
	return &MessageEntry{
		EntryID:           fmt.Sprintf("%d", msg.Identifier),
		FolderPath:        folderPath,
		Subject:           subject,
		TransportHeaders:  transportHeaders,
		BodyText:          bodyText,
		BodyHTML:          bodyHTML,
		RawHTMLAttachment: rawHTML,
		BodyError:         bodyErr,
		SenderName:        senderName,
		SenderEmail:       senderEmail,
		SenderAddressType: senderType,
		AuthorName:        authorName,
		AuthorEmail:       authorEmail,
		UnresolvedSender:  unresolvedSender,
		UnresolvedAuthor:  unresolvedAuthor,
		Recipients:        recipients,
		HeaderError:       errors.Join(headerErrors...),
		DisplayTo:         displayTo,
		DisplayCc:         displayCc,
		DisplayBcc:        displayBcc,
		MessageID:         messageID,
		InReplyTo:         inReplyTo,
		References:        references,
		SentAt:            sentAt,
		ReceivedAt:        receivedAt,
		CreationTime:      createdAt,
		DateError:         errors.Join(sentErr, receivedErr, createdErr),
	}
}

// resolveMessageIdentity prefers a usable SMTP override to an Exchange value.
// An unresolved Exchange value is retained separately instead of being used as
// an email address or being treated as a property read error.
func resolveMessageIdentity(name, addrType, email, smtp string) (string, *UnresolvedIdentity) {
	if isExchangeDN(email) || strings.EqualFold(addrType, "EX") {
		if parsed, err := mail.ParseAddress(strings.TrimSpace(smtp)); err == nil && strings.Contains(parsed.Address, "@") {
			return parsed.Address, nil
		}
		return "", &UnresolvedIdentity{Name: name, AddressType: addrType, RawAddress: email, RawSMTP: smtp}
	}
	if smtp != "" {
		return smtp, nil
	}
	return email, nil
}

var errAttachmentTooLarge = errors.New("attachment exceeds size limit")

// limitWriter wraps an io.Writer and returns errAttachmentTooLarge once the
// remaining byte budget is exhausted.
type limitWriter struct {
	w         io.Writer
	remaining int64
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > lw.remaining {
		return 0, errAttachmentTooLarge
	}
	n, err := lw.w.Write(p)
	lw.remaining -= int64(n)
	return n, err
}

// noAttachments reports whether err from GetAttachmentIterator means the
// message has no attachments to read: either its has-attachments flag is
// unset, or the flag is set but its attachment table has no rows.
func noAttachments(err error) bool {
	return eris.Is(err, pstlib.ErrAttachmentsNotFound) ||
		eris.Is(err, pstlib.ErrTableContextNoRows)
}

// ReadAttachments reads all attachments from a pstlib.Message into memory.
// Returns an empty slice (not an error) when there are no attachments,
// including a message flagged with attachments whose table has no rows.
// Individual attachment read errors are returned as a non-nil error.
func ReadAttachments(msg *pstlib.Message, maxBytes int64) ([]AttachmentEntry, error) {
	iter, err := msg.GetAttachmentIterator()
	if noAttachments(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get attachment iterator: %w", err)
	}

	var attachments []AttachmentEntry
	var totalBytes int64

	for iter.Next() {
		att := iter.Value()

		filename := att.GetAttachLongFilename()
		if filename == "" {
			filename = att.GetAttachFilename()
		}

		mimeType := att.GetAttachMimeTag()
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}

		// Pre-check using the reported size when available; the bounded writer below
		// enforces the limit unconditionally, covering the size==0 case.
		if maxBytes > 0 {
			estimatedSize := int64(att.GetAttachSize())
			if estimatedSize > maxBytes-totalBytes {
				return nil, fmt.Errorf("read attachment %q: %w", filename, errAttachmentTooLarge)
			}
		}

		// Stream attachment content through a bounded writer so a corrupted or
		// malicious PST reporting size 0 cannot exhaust memory.
		var buf bytes.Buffer
		w := io.Writer(&buf)
		if maxBytes > 0 {
			w = &limitWriter{w: &buf, remaining: maxBytes - totalBytes}
		}
		written, err := att.WriteTo(w)
		if err != nil {
			return nil, fmt.Errorf("read attachment %q: %w", filename, err)
		}
		totalBytes += written

		attachments = append(attachments, AttachmentEntry{
			Filename:  filename,
			MIMEType:  mimeType,
			ContentID: att.GetAttachContentId(),
			Size:      att.GetAttachSize(),
			Content:   buf.Bytes(),
		})
	}

	if err := iter.Err(); err != nil {
		return attachments, fmt.Errorf("attachment iterator: %w", err)
	}
	return attachments, nil
}

// isExchangeDN reports whether s looks like an Exchange Distinguished Name.
func isExchangeDN(s string) bool {
	return strings.HasPrefix(s, "/O=") || strings.HasPrefix(s, "/o=")
}

// extractCN extracts the last CN= component from an Exchange DN.
// E.g. "/O=CORP/OU=EXCHANGE/CN=RECIPIENTS/CN=JSMITH" → "JSMITH".
func extractCN(dn string) string {
	parts := strings.Split(dn, "/")
	for _, v := range slices.Backward(parts) {
		p := v
		if upper := strings.ToUpper(p); strings.HasPrefix(upper, "CN=") {
			return p[3:]
		}
	}
	return dn
}
