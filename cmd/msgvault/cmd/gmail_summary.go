package cmd

import (
	"fmt"
	"io"

	"go.kenn.io/msgvault/internal/gmail"
)

func writeGmailSyncErrors(out io.Writer, summary *gmail.SyncSummary) {
	if summary.Errors > 0 || summary.UnresolvedMessages > 0 {
		_, _ = fmt.Fprintf(out, "  Error attempts: %d\n", summary.Errors)
		_, _ = fmt.Fprintf(out, "  Unresolved:    %d", summary.UnresolvedMessages)
		if summary.UnresolvedMessages == 0 {
			_, _ = fmt.Fprint(out, " (all current work settled)")
		}
		_, _ = fmt.Fprintln(out)
	}
}
