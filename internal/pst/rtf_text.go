package pst

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"golang.org/x/net/html/charset"
)

const maxRTFGroups = 512

// errRTFIncompleteText indicates that readable text was recovered, but an
// unsupported destination may contain additional visible text.
var errRTFIncompleteText = errors.New("RTF searchable text may be incomplete: visible content was not extracted")

// extractRTFText extracts a conservative plain-text view of an RTF body. It
// returns no text when the syntax or character encoding cannot be interpreted
// reliably. When visible destinations are omitted, it returns the recovered
// text and errRTFIncompleteText; callers retain the original RTF in either case.
func extractRTFText(data []byte, maxBytes int) (string, error) {
	// Some Outlook RTF properties include terminal NUL padding after the root
	// group. It is not part of the RTF text and must not hide a valid body.
	data = bytes.Trim(data, " \t\r\n\x00")
	if maxBytes <= 0 || !bytes.HasPrefix(data, []byte(`{\rtf1`)) || len(data) > 6 && (rtfLetter(data[6]) || data[6] >= '0' && data[6] <= '9' || data[6] == '-') {
		return "", errors.New("invalid RTF header or output limit")
	}
	p := rtfTextParser{data: data, limit: maxBytes, defaultFont: -1, fontCharsets: make(map[int]int), state: rtfTextState{codepage: 1252, uc: 1, font: -1, fontDefinition: -1}}
	if err := p.parse(); err != nil {
		return "", err
	}
	if p.incompleteText {
		return p.output.String(), errRTFIncompleteText
	}
	return p.output.String(), nil
}

type rtfTextState struct {
	skip, hidden, deleted, fontTable bool
	codepage, font, fontDefinition   int
	uc                               int
}

type rtfTextParser struct {
	data           []byte
	limit          int
	state          rtfTextState
	stack          []rtfTextState
	output         strings.Builder
	pending        []byte
	fallback       int
	high           uint16
	rootClosed     bool
	incompleteText bool
	defaultFont    int
	fontCharsets   map[int]int
}

func (p *rtfTextParser) visible() bool { return !p.state.skip && !p.state.hidden && !p.state.deleted }

func (p *rtfTextParser) appendText(s string) error {
	if len(s) > p.limit-p.output.Len() {
		return errors.New("RTF text exceeds output limit")
	}
	p.output.WriteString(s)
	return nil
}

func (p *rtfTextParser) flushBytes() error {
	if len(p.pending) == 0 {
		return nil
	}
	if p.high != 0 {
		return errors.New("unpaired RTF Unicode surrogate")
	}
	codepage := p.state.codepage
	if c, ok := p.fontCharsets[p.state.font]; ok && c != 0 {
		codepage = c
	}
	name := pstlib.CodePageIdentifierToEncoding[codepage]
	if codepage == 932 {
		name = "shift_jis"
	}
	enc, _ := charset.Lookup(name)
	if enc == nil {
		return fmt.Errorf("unsupported RTF codepage %d", codepage)
	}
	decoded, err := enc.NewDecoder().Bytes(p.pending)
	if err != nil || !utf8.Valid(decoded) || bytes.Contains(decoded, []byte("\ufffd")) && !bytes.Contains(p.pending, []byte("\ufffd")) {
		return errors.New("undecodable RTF text")
	}
	p.pending = p.pending[:0]
	return p.appendText(string(decoded))
}

func (p *rtfTextParser) emitByte(b byte) error {
	if p.fallback > 0 {
		p.fallback--
		return nil
	}
	if !p.visible() {
		return nil
	}
	if b < 0x20 || b == 0x7f {
		return errors.New("unexpected RTF text control byte")
	}
	if p.high != 0 {
		return errors.New("unpaired RTF Unicode surrogate")
	}
	p.pending = append(p.pending, b)
	if len(p.pending) > p.limit {
		return errors.New("RTF text exceeds output limit")
	}
	return nil
}

func (p *rtfTextParser) emitRune(r rune) error {
	if p.fallback > 0 {
		p.fallback--
		return nil
	}
	if !p.visible() {
		return nil
	}
	if err := p.flushBytes(); err != nil {
		return err
	}
	if p.high != 0 {
		return errors.New("unpaired RTF Unicode surrogate")
	}
	return p.appendText(string(r))
}

func (p *rtfTextParser) emitUnicode(value int) error {
	if value < -32768 || value > 65535 {
		return errors.New("invalid RTF Unicode value")
	}
	if err := p.flushBytes(); err != nil {
		return err
	}
	if p.visible() {
		unit := uint16(value)
		switch {
		case unit >= 0xd800 && unit <= 0xdbff:
			if p.high != 0 {
				return errors.New("unpaired RTF Unicode surrogate")
			}
			p.high = unit
		case unit >= 0xdc00 && unit <= 0xdfff:
			if p.high == 0 {
				return errors.New("unpaired RTF Unicode surrogate")
			}
			r := utf16.DecodeRune(rune(p.high), rune(unit))
			p.high = 0
			if err := p.appendText(string(r)); err != nil {
				return err
			}
		default:
			if p.high != 0 {
				return errors.New("unpaired RTF Unicode surrogate")
			}
			if unit < 0x20 || unit == 0x7f {
				return errors.New("invalid RTF Unicode control")
			}
			if err := p.appendText(string(rune(unit))); err != nil {
				return err
			}
		}
	}
	p.fallback = p.state.uc
	return nil
}

// rtfDestination marks metadata, embedded objects and formatting tables, not
// the message's visible body. Unknown ignorable destinations use \\*.
func rtfDestination(word string) bool {
	switch word {
	case "fonttbl", "colortbl", "stylesheet", "info", "generator", "filetbl", "listtable", "listoverridetable", "revtbl", "rsidtbl", "xmlnstbl", "themedata", "datastore", "colorschememapping", "pict", "object", "objdata", "objclass", "objname", "nonshppict", "shppict", "shp", "shpinst", "shptxt", "fldinst", "header", "headerl", "headerr", "headerf", "footer", "footerl", "footerr", "footerf", "footnote", "annotation", "atnauthor", "atnid", "pntext", "listtext", "latentstyles", "xmlopen", "xmlclose", "htmltag", "mhtmltag":
		return true
	default:
		return false
	}
}

// These destinations can contain user-visible document content. We do not
// parse them as ordinary body text, so their presence must be reported.
func rtfPotentiallyVisibleDestination(word string) bool {
	switch word {
	case "shp", "shpinst", "shptxt", "object", "header", "headerl", "headerr", "headerf", "footer", "footerl", "footerr", "footerf", "footnote", "annotation", "pntext", "listtext":
		return true
	default:
		return false
	}
}

func (p *rtfTextParser) parse() error {
	for i := 0; i < len(p.data); {
		b := p.data[i]
		i++
		switch b {
		case '{':
			if p.rootClosed || len(p.stack) >= maxRTFGroups {
				return errors.New("invalid or excessive RTF groups")
			}
			if err := p.flushBytes(); err != nil {
				return err
			}
			p.stack = append(p.stack, p.state)
		case '}':
			if len(p.stack) == 0 || p.fallback != 0 {
				return errors.New("invalid RTF group closure")
			}
			if err := p.flushBytes(); err != nil {
				return err
			}
			p.state = p.stack[len(p.stack)-1]
			p.stack = p.stack[:len(p.stack)-1]
			if len(p.stack) == 0 {
				p.rootClosed = true
			}
		case '\r', '\n':
			// Raw line breaks are insignificant in RTF source.
		case '\\':
			if len(p.stack) == 0 || i >= len(p.data) {
				return errors.New("truncated RTF control")
			}
			var err error
			i, err = p.control(i)
			if err != nil {
				return err
			}
		default:
			if len(p.stack) == 0 {
				return errors.New("RTF text outside root group")
			}
			if err := p.emitByte(b); err != nil {
				return err
			}
		}
	}
	if len(p.stack) != 0 || !p.rootClosed || p.high != 0 || p.fallback != 0 {
		return errors.New("incomplete RTF")
	}
	return nil
}

func (p *rtfTextParser) control(i int) (int, error) {
	c := p.data[i]
	i++
	if !rtfLetter(c) {
		switch c {
		case '\\', '{', '}':
			return i, p.emitByte(c)
		case '~':
			return i, p.emitRune('\u00a0')
		case '_':
			return i, p.emitRune('\u2011')
		case '-':
			return i, p.emitRune('\u00ad')
		case '*':
			if err := p.flushBytes(); err != nil {
				return i, err
			}
			p.state.skip = true
			return i, nil
		case '\'':
			if i+2 > len(p.data) {
				return i, errors.New("truncated RTF hex escape")
			}
			value, err := strconv.ParseUint(string(p.data[i:i+2]), 16, 8)
			if err != nil {
				return i, errors.New("invalid RTF hex escape")
			}
			return i + 2, p.emitByte(byte(value))
		case '\r', '\n':
			return i, nil
		default:
			return i, errors.New("unsupported RTF control symbol")
		}
	}
	start := i - 1
	for i < len(p.data) && rtfLetter(p.data[i]) {
		i++
	}
	word := string(p.data[start:i])
	start = i
	if i < len(p.data) && (p.data[i] == '-' || p.data[i] >= '0' && p.data[i] <= '9') {
		if p.data[i] == '-' {
			i++
		}
		for i < len(p.data) && p.data[i] >= '0' && p.data[i] <= '9' {
			i++
		}
	}
	hasValue := i > start
	value := 0
	if hasValue {
		var err error
		value, err = strconv.Atoi(string(p.data[start:i]))
		if err != nil {
			return i, errors.New("invalid RTF control parameter")
		}
	}
	if i < len(p.data) && p.data[i] == ' ' {
		i++
	}
	if word == "bin" {
		if !hasValue || value < 0 || value > len(p.data)-i || p.fallback != 0 {
			return i, errors.New("invalid RTF binary length")
		}
		return i + value, nil
	}
	if word == "fromhtml" || word == "upr" {
		return i, errors.New("unsupported RTF encapsulated or alternate content")
	}
	if word == "fonttbl" {
		if err := p.flushBytes(); err != nil {
			return i, err
		}
		p.state.skip = true
		p.state.fontTable = true
		return i, nil
	}
	if p.state.fontTable {
		switch word {
		case "f":
			if hasValue && value >= 0 {
				p.state.fontDefinition = value
			}
		case "fcharset":
			if hasValue && p.state.fontDefinition >= 0 {
				p.fontCharsets[p.state.fontDefinition] = rtfFontCodepage(value)
			}
		}
	}
	if rtfPotentiallyVisibleDestination(word) {
		p.incompleteText = true
	}
	if rtfDestination(word) {
		if err := p.flushBytes(); err != nil {
			return i, err
		}
		p.state.skip = true
		return i, nil
	}
	if p.state.skip {
		return i, nil
	}
	switch word {
	case "mac", "pc", "pca":
		return i, errors.New("unsupported RTF default charset")
	case "ansicpg":
		if !hasValue {
			return i, errors.New("missing RTF codepage")
		}
		if err := p.flushBytes(); err != nil {
			return i, err
		}
		p.state.codepage = value
	case "deff":
		if hasValue && value >= 0 {
			p.defaultFont = value
			p.state.font = value
		}
	case "f":
		if hasValue && value >= 0 {
			if err := p.flushBytes(); err != nil {
				return i, err
			}
			p.state.font = value
		}
	case "uc":
		if !hasValue || value < 0 || value > 32 {
			return i, errors.New("invalid RTF Unicode fallback count")
		}
		p.state.uc = value
	case "u":
		if !hasValue || p.fallback != 0 {
			return i, errors.New("invalid RTF Unicode escape")
		}
		return i, p.emitUnicode(value)
	case "v", "deleted":
		if err := p.flushBytes(); err != nil {
			return i, err
		}
		if word == "v" {
			p.state.hidden = !hasValue || value != 0
		} else {
			p.state.deleted = !hasValue || value != 0
		}
	case "plain":
		if err := p.flushBytes(); err != nil {
			return i, err
		}
		p.state.hidden = false
		p.state.font = p.defaultFont
	case "par", "line", "row":
		return i, p.emitRune('\n')
	case "tab", "cell":
		return i, p.emitRune('\t')
	case "emdash":
		return i, p.emitRune('\u2014')
	case "endash":
		return i, p.emitRune('\u2013')
	case "bullet":
		return i, p.emitRune('\u2022')
	case "lquote":
		return i, p.emitRune('\u2018')
	case "rquote":
		return i, p.emitRune('\u2019')
	case "ldblquote":
		return i, p.emitRune('\u201c')
	case "rdblquote":
		return i, p.emitRune('\u201d')
	}
	return i, nil
}

func rtfLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

// RTF's font charset identifies a codepage independently of the document
// default. Unknown encodings are rejected instead of guessing glyphs.
func rtfFontCodepage(value int) int {
	switch value {
	case 0, 1:
		return 0 // ANSI or default uses the document codepage.
	case 128:
		return 932
	case 129:
		return 949
	case 134:
		return 936
	case 136:
		return 950
	case 161:
		return 1253
	case 162:
		return 1254
	case 163:
		return 1258
	case 177:
		return 1255
	case 178:
		return 1256
	case 186:
		return 1257
	case 204:
		return 1251
	case 222:
		return 874
	case 238:
		return 1250
	default:
		return -1
	}
}
