package mime

import "github.com/jhillyerd/enmime/v2"

// The DOM converter's nesting limit affects only derived plain text. The
// original HTML and MIME parts are retained, and GetBodyText already uses
// StripHTML for this case. Other severe errors still mean content was lost.
func isHTMLDepthConversionError(e *enmime.Error) bool {
	return e.Severe && e.Name == enmime.ErrorPlainTextFromHTML &&
		e.Detail == "Failed to downconvert HTML: html: open stack of elements exceeds 512 nodes"
}
