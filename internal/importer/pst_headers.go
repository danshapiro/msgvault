package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
)

// pstMessageIngester stores a new PST message and then records the threading
// facts that ReconcilePstEmailThreadsContext reads when the import finishes.
func pstMessageIngester(images *remoteimage.Fetcher) rawMessageIngestFunc {
	return func(
		ctx context.Context, st *store.Store, sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string, raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error {
		if err := ingestRawMessageWithCompletion(ctx, st, sourceID, identifier, attachmentsDir, labelIDs, sourceMsgID,
			rawHash, raw, fallbackDate, log, images, "", false, unresolvedPSTSender(raw)); err != nil {
			return err
		}
		return recordPstMessageHeaders(ctx, st, sourceID, sourceMsgID, raw)
	}
}

type unresolvedPSTIdentity struct {
	Name        string `json:"name"`
	AddressType string `json:"address_type"`
	RawAddress  string `json:"address"`
}

// unresolvedPSTSender reads only the PST importer's encoded metadata.
// It prefers the represented author, as the synthesized From header does.
// Full values, including controls, remain intact in the original metadata.
func unresolvedPSTSender(raw []byte) *unresolvedPSTIdentity {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	for _, key := range []string{"X-Msgvault-Unresolved-Author", "X-Msgvault-Unresolved-Sender"} {
		values := msg.Header[key]
		if len(values) == 0 {
			continue
		}
		// BuildRFC5322 appends its generated metadata after transport fields.
		encoded := strings.Join(strings.Fields(values[len(values)-1]), "")
		data, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		var identity unresolvedPSTIdentity
		if err := json.Unmarshal(data, &identity); err != nil {
			continue
		}
		identity.Name = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, identity.Name))
		if identity.Name != "" {
			return &identity
		}
	}
	return nil
}

// unresolvedPSTRecipientRoles reports source recipient facts that could not
// be represented as SMTP addresses. Values remain in the encoded MIME header;
// the repair uses these roles only to inform the operator.
func unresolvedPSTRecipientRoles(raw []byte) []string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	var roles []string
	for _, value := range msg.Header["X-Msgvault-Unresolved-Recipient"] {
		data, err := base64.RawURLEncoding.DecodeString(strings.Join(strings.Fields(value), ""))
		var identity struct {
			Role int32 `json:"role"`
		}
		if err != nil || json.Unmarshal(data, &identity) != nil {
			roles = append(roles, "recipient metadata unreadable")
			continue
		}
		switch identity.Role {
		case 1:
			roles = append(roles, "to-unresolved")
		case 2:
			roles = append(roles, "cc-unresolved")
		case 3:
			roles = append(roles, "bcc-unresolved")
		default:
			roles = append(roles, "recipient-unresolved")
		}
	}
	return roles
}

// participantKey shares a confidently identified Exchange identity within a
// source. Ambiguous identities remain scoped to one message to avoid merging
// unrelated people. The hash keeps the original DN out of the identifier.
func (identity *unresolvedPSTIdentity) participantKey(sourceID int64, sourceMsgID string) string {
	if canonical, ok := canonicalExchangeDN(identity.AddressType, identity.RawAddress); ok {
		digest := sha256.Sum256([]byte(canonical))
		return fmt.Sprintf("%d:dn:%x", sourceID, digest)
	}
	return fmt.Sprintf("%d:message:%s", sourceID, sourceMsgID)
}

// canonicalExchangeDN recognizes only unambiguous legacy Exchange paths.
// It folds ASCII case without changing non-ASCII characters or whitespace.
func canonicalExchangeDN(addressType, address string) (string, bool) {
	if (addressType != "" && !strings.EqualFold(addressType, "EX")) || !utf8.ValidString(address) || !strings.HasPrefix(address, "/") {
		return "", false
	}
	for _, r := range address {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	parts := strings.Split(address[1:], "/")
	if len(parts) < 2 {
		return "", false
	}
	hasCN := false
	for i, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok || value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, `\=`) {
			return "", false
		}
		switch {
		case i == 0 && strings.EqualFold(key, "O"):
		case i > 0 && !hasCN && strings.EqualFold(key, "OU"):
		case i > 0 && strings.EqualFold(key, "CN"):
			hasCN = true
		default:
			return "", false
		}
	}
	if !hasCN {
		return "", false
	}
	canonical := []byte(address)
	for i, c := range canonical {
		if c >= 'A' && c <= 'Z' {
			canonical[i] = c + ('a' - 'A')
		}
	}
	return string(canonical), true
}

func recordPstMessageHeaders(ctx context.Context, st *store.Store, sourceID int64, sourceMsgID string, raw []byte) error {
	ids, err := st.MessageExistsBatch(sourceID, []string{sourceMsgID})
	if err != nil {
		return fmt.Errorf("find imported PST message: %w", err)
	}
	id, ok := ids[sourceMsgID]
	if !ok {
		return fmt.Errorf("imported PST message %q missing from source %d", sourceMsgID, sourceID)
	}
	return recordPstMessageHeadersByID(ctx, st, sourceID, id, raw)
}

func recordPstMessageHeadersByID(ctx context.Context, st *store.Store, sourceID, messageID int64, raw []byte) error {
	rfcID, parent, refs := mime.ParseThreadingHeaders(raw)
	key := rfcID
	if parent != "" {
		key = parent
	}
	if len(refs) > 0 {
		if root := mime.NormalizeMessageID(refs[0]); root != "" {
			key = root
		}
	}
	if err := st.RecordPstEmailHeadersContext(ctx, sourceID, messageID, rfcID, parent, key); err != nil {
		return fmt.Errorf("record PST headers for message %d: %w", messageID, err)
	}
	return nil
}
