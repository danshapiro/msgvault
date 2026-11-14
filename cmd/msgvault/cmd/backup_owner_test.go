package cmd

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackupOwnerInputCancellation(t *testing.T) {
	for _, mode := range []string{"eof", "read-error", "parent-cancel", "success", "command-failure", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			input, writer := io.Pipe()
			defer func() { _ = writer.Close() }()
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			cmd := &cobra.Command{}
			cmd.SetContext(parent)
			cmd.SetIn(input)
			cmd.Flags().Bool(backupOwnerInputFlag, mode != "disabled", "")
			entered := make(chan struct{})
			done := make(chan error, 1)
			commandFailure := errors.New("command failed")
			go func() {
				done <- withBackupOwnerInput(cmd, func() error {
					close(entered)
					if mode == "success" || mode == "disabled" {
						return nil
					}
					if mode == "command-failure" {
						return commandFailure
					}
					<-cmd.Context().Done()
					return cmd.Context().Err()
				})
			}()
			<-entered
			switch mode {
			case "eof":
				require.NoError(writer.Close())
			case "read-error":
				require.NoError(writer.CloseWithError(io.ErrUnexpectedEOF))
			case "parent-cancel":
				cancel()
			}
			select {
			case err := <-done:
				switch mode {
				case "success", "disabled":
					require.NoError(err)
				case "command-failure":
					require.ErrorIs(err, commandFailure)
				case "eof":
					require.ErrorIs(err, errBackupOwnerInputClosed)
				case "read-error":
					require.ErrorIs(err, io.ErrUnexpectedEOF)
				case "parent-cancel":
					require.ErrorIs(err, context.Canceled)
				}
			case <-time.After(5 * time.Second):
				require.FailNow("owner input cleanup did not finish")
			}
			assert.Equal(parent, cmd.Context(), "caller context restored")
			if mode == "disabled" {
				_ = input.Close()
			}
		})
	}
}
