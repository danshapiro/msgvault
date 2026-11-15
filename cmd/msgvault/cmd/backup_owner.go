package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"
)

const backupOwnerInputFlag = "cancel-on-stdin-eof"

var errBackupOwnerInputClosed = errors.New("backup owner input closed")

// withBackupOwnerInput keeps remote execution tied to the invoking process.
// The opt-in input belongs exclusively to this command and is closed on exit.
// The daemon child uses its request context, never its own closed stdin.
func withBackupOwnerInput(cmd *cobra.Command, run func() error) error {
	enabled, err := cmd.Flags().GetBool(backupOwnerInputFlag)
	if err != nil {
		return fmt.Errorf("read backup owner input flag: %w", err)
	}
	if !enabled {
		return run()
	}
	input, ok := cmd.InOrStdin().(io.ReadCloser)
	if !ok {
		return errors.New("backup owner input must be closeable")
	}
	ctx, cancel := context.WithCancelCause(cmd.Context())
	closeInput := sync.OnceFunc(func() { _ = input.Close() })
	defer closeInput()
	read, interrupt, err := backupOwnerInputReader(ctx, input, closeInput)
	if err != nil {
		cancel(err)
		return err
	}
	done := make(chan struct{})
	go func() {
		readErr := read()
		if readErr != nil {
			cancel(fmt.Errorf("backup owner input: %w", readErr))
		} else {
			cancel(errBackupOwnerInputClosed)
		}
		close(done)
	}()
	previous := cmd.Context()
	cmd.SetContext(ctx)
	defer func() {
		cancel(context.Canceled)
		interrupt()
		<-done
		cmd.SetContext(previous)
	}()
	err = run()
	if ctx.Err() != nil {
		return errors.Join(context.Cause(ctx), err)
	}
	return err
}

// In-memory/other-platform readers use their Close contract to interrupt Read.
// Unix inherited descriptors instead use a cancellable poll loop (see the
// platform helper); Close alone cannot interrupt an inherited blocking read.
func backupOwnerCloseReader(input io.ReadCloser, closeInput func()) (func() error, func()) {
	return func() error {
		_, err := io.Copy(io.Discard, input)
		return err
	}, closeInput
}
