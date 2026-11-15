//go:build !windows

package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An inherited pipe is a blocking os.Stdin, unlike io.Pipe or os.Pipe in
// the same Go process. Keep its writer open while the operation completes.
func TestBackupOwnerInheritedInput(t *testing.T) {
	if mode := os.Getenv("MSGVAULT_TEST_BACKUP_OWNER_INPUT"); mode != "" {
		require := require.New(t)
		cmd := &cobra.Command{}
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		cmd.SetContext(parent)
		cmd.Flags().Bool(backupOwnerInputFlag, true, "")
		failure := errors.New("operation failed")
		err := withBackupOwnerInput(cmd, func() error {
			fmt.Println("operation-started")
			if mode == "eof" || mode == "parent-cancel" {
				if mode == "parent-cancel" {
					cancel()
				}
				<-cmd.Context().Done()
				return cmd.Context().Err()
			}
			// Simulate completion while the inherited pipe stays open. This reader
			// is an operating-system event and cannot use a synctest bubble.
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			<-timer.C
			if mode == "failure" {
				return failure
			}
			return nil
		})
		switch mode {
		case "success":
			require.NoError(err)
		case "failure":
			require.ErrorIs(err, failure)
		case "eof":
			require.ErrorIs(err, errBackupOwnerInputClosed)
		case "parent-cancel":
			require.ErrorIs(err, context.Canceled)
		default:
			require.FailNow("unknown subprocess case")
		}
		assert.Equal(t, parent, cmd.Context())
		return
	}
	for _, mode := range []string{"success", "failure", "eof", "parent-cancel"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			executable, err := os.Executable()
			require.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestBackupOwnerInheritedInput$", "-test.count=1")
			child.Env = append(os.Environ(), "MSGVAULT_TEST_BACKUP_OWNER_INPUT="+mode)
			writer, err := child.StdinPipe()
			require.NoError(err)
			defer func() { _ = writer.Close() }()
			output, err := child.StdoutPipe()
			require.NoError(err)
			child.Stderr = os.Stderr
			require.NoError(child.Start())
			scanner := bufio.NewScanner(output)
			require.True(scanner.Scan(), "child must enter the real operation")
			require.Equal("operation-started", scanner.Text())
			if mode == "eof" {
				require.NoError(writer.Close())
			}
			for scanner.Scan() {
				t.Log(scanner.Text())
			}
			require.NoError(scanner.Err())
			require.NoError(child.Wait(), "operation must return while the owner pipe remains open")
			require.NoError(ctx.Err(), "cleanup must finish without waiting for owner EOF")
		})
	}
}
