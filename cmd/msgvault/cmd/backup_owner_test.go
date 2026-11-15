package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackupOwnerRedirectedInputEOF(t *testing.T) {
	for _, mode := range []string{"regular-file", "null-device"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			path := os.DevNull
			if mode == "regular-file" {
				path = filepath.Join(t.TempDir(), "owner-input")
				require.NoError(os.WriteFile(path, []byte("owner input\n"), 0600))
			}
			input, err := os.Open(path)
			require.NoError(err)
			t.Cleanup(func() { _ = input.Close() })
			parent, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := &cobra.Command{}
			cmd.SetContext(parent)
			cmd.SetIn(input)
			cmd.Flags().Bool(backupOwnerInputFlag, true, "")
			err = withBackupOwnerInput(cmd, func() error {
				<-cmd.Context().Done()
				return cmd.Context().Err()
			})
			require.ErrorIs(err, errBackupOwnerInputClosed)
			require.NoError(parent.Err(), "input EOF must cancel before the parent deadline")
			assert.Equal(t, parent, cmd.Context())
			_, err = input.Read(make([]byte, 1))
			assert.ErrorIs(t, err, os.ErrClosed, "the command owns and closes the input")
		})
	}
}

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
