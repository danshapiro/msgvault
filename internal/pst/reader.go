package pst

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	charsets "github.com/emersion/go-message/charset"
	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/rotisserie/eris"
	"golang.org/x/text/encoding"
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
	BodyText string
	BodyHTML string

	// Sender fields.
	SenderName        string
	SenderEmail       string
	SenderAddressType string // "SMTP" or "EX" (Exchange DN)

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

	subject := props.GetSubject()
	if subject == "" {
		subject = props.GetNormalizedSubject()
	}
	if subject == "" {
		subject = props.GetInternetSubject()
	}

	senderEmail := props.GetSenderEmailAddress()
	// Exchange Distinguished Names start with /O= — try to resolve to SMTP.
	if isExchangeDN(senderEmail) {
		if smtp := props.GetSmtpAddress(); smtp != "" {
			senderEmail = smtp
		} else {
			senderEmail = extractCN(senderEmail)
		}
	}

	sentAt, sentErr := messageDate(msg, 0x0039, props.ClientSubmitTime)
	receivedAt, receivedErr := messageDate(msg, 0x0e06, props.MessageDeliveryTime)
	createdAt, createdErr := messageDate(msg, 0x3007, props.CreationTime)
	return &MessageEntry{
		EntryID:           fmt.Sprintf("%d", msg.Identifier),
		FolderPath:        folderPath,
		Subject:           subject,
		TransportHeaders:  props.GetTransportMessageHeaders(),
		BodyText:          props.GetBody(),
		BodyHTML:          props.GetBodyHtml(),
		SenderName:        props.GetSenderName(),
		SenderEmail:       senderEmail,
		SenderAddressType: props.GetSenderAddressType(),
		DisplayTo:         props.GetDisplayTo(),
		DisplayCc:         props.GetDisplayCc(),
		DisplayBcc:        props.GetDisplayBcc(),
		MessageID:         props.GetInternetMessageId(),
		InReplyTo:         props.GetInReplyToId(),
		References:        props.GetInternetReferences(),
		SentAt:            sentAt,
		ReceivedAt:        receivedAt,
		CreationTime:      createdAt,
		DateError:         errors.Join(sentErr, receivedErr, createdErr),
	}
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
