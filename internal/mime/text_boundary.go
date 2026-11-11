package mime

import (
	"bytes"
	"encoding/base64"
	"io"
	stdmime "mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"

	"github.com/jhillyerd/enmime/v2"
)

const textBoundaryErrorDetail = "content is text or data but has a boundary marker"

// downgradeUnusedTextBoundaries retains enmime's diagnostic as a warning when
// the declared delimiter is provably unused. enmime decoded these leaves as
// text already; only its claim that content was lost changes.
func downgradeUnusedTextBoundaries(raw []byte, root *enmime.Part) {
	parts := root.DepthMatchAll(func(*enmime.Part) bool { return true })
	var problems []*enmime.Error
	candidates := make(map[*enmime.Part]bool)
	for _, parent := range parts {
		boundaryErrors := 0
		for _, problem := range parent.Errors {
			if !problem.Severe {
				continue
			}
			if problem.Name != enmime.ErrorDataHasBoundary || problem.Detail != textBoundaryErrorDetail {
				return
			}
			problems = append(problems, problem)
			boundaryErrors++
		}
		textLeaves := 0
		for child := parent.FirstChild; child != nil; child = child.NextSibling {
			if isTextBoundaryLeaf(child) {
				candidates[child] = true
				textLeaves++
			}
		}
		if textLeaves != boundaryErrors {
			return
		}
	}
	if len(problems) == 0 {
		return
	}

	rawMessage, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return
	}
	valid := walkRawParts(rawMessage.Body, root, func(body io.Reader, part *enmime.Part) bool {
		if !candidates[part] {
			_, err := io.Copy(io.Discard, body)
			return err == nil
		}
		return unusedTextBoundary(body, part)
	}, nil)
	if !valid {
		return
	}
	for _, problem := range problems {
		problem.Severe = false
	}
}

func isTextBoundaryLeaf(part *enmime.Part) bool {
	return part.FirstChild == nil && part.Boundary != "" &&
		(part.ContentType == "text/plain" || part.ContentType == "text/html")
}

func unusedTextBoundary(body io.Reader, part *enmime.Part) bool {
	if len(part.Header.Values("Content-Type")) != 1 || len(part.Header.Values("Content-Transfer-Encoding")) > 1 {
		return false
	}
	contentType, params, err := stdmime.ParseMediaType(part.Header.Get("Content-Type"))
	if err != nil || contentType != part.ContentType || params["boundary"] != part.Boundary {
		return false
	}
	raw, err := io.ReadAll(body)
	if err != nil || containsDelimiterLine(raw, part.Boundary) {
		return false
	}
	var decoded io.Reader = bytes.NewReader(raw)
	switch strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding"))) {
	case "", "7bit", "8bit", "binary":
	case "base64":
		decoded = base64.NewDecoder(base64.StdEncoding, decoded)
	case "quoted-printable":
		decoded = quotedprintable.NewReader(decoded)
	default:
		return false
	}
	content, err := io.ReadAll(decoded)
	return err == nil && !containsDelimiterLine(content, part.Boundary)
}

func containsDelimiterLine(content []byte, boundary string) bool {
	opening, closing := []byte("--"+boundary), []byte("--"+boundary+"--")
	for line := range bytes.SplitSeq(content, []byte{'\n'}) {
		line = bytes.TrimRight(line, "\r \t")
		if bytes.Equal(line, opening) || bytes.Equal(line, closing) {
			return true
		}
	}
	return false
}
