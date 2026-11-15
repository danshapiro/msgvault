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

// A caller's Close may have side effects; cancellation and final cleanup must
// share one close even when the reader is not an operating-system file.
type countingBackupOwnerInput struct {
	*io.PipeReader

	closes int
}

func (input *countingBackupOwnerInput) Close() error {
	input.closes++
	return input.PipeReader.Close()
}

func TestBackupOwnerInputClosesOnce(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	input := &countingBackupOwnerInput{PipeReader: reader}
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetIn(input)
	command.Flags().Bool(backupOwnerInputFlag, true, "")
	require.NoError(t, withBackupOwnerInput(command, func() error { return nil }))
	assert.Equal(t, 1, input.closes)
}
