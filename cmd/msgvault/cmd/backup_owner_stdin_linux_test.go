//go:build linux

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
	"golang.org/x/sys/unix"
)

// Exercise inherited blocking fd 0, not io.Pipe or Go's pollable os.Pipe file.
// The parent retains its writer until the child exits, as an owner bridge does.
func TestBackupOwnerInheritedStdin(t *testing.T) {
	for _, mode := range []string{"success", "command-failure", "parent-cancel", "eof"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			input, writer, err := os.Pipe()
			require.NoError(err)
			t.Cleanup(func() { _ = input.Close(); _ = writer.Close() })
			control, release, err := os.Pipe()
			require.NoError(err)
			t.Cleanup(func() { _ = control.Close(); _ = release.Close() })
			// Ensure the child reader actually consumes input before run returns.
			_, err = writer.WriteString("owner still alive")
			require.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			executable, err := os.Executable()
			require.NoError(err)
			child := exec.CommandContext(ctx, executable, "-test.run=^TestBackupOwnerInheritedStdinChild$")
			child.Env = append(os.Environ(), "MSGVAULT_TEST_OWNER_STDIN="+mode)
			child.Stdin = input
			child.ExtraFiles = []*os.File{control}
			stdout, err := child.StdoutPipe()
			require.NoError(err)
			child.Stderr = os.Stderr
			require.NoError(child.Start())
			t.Cleanup(func() { cancel(); _ = child.Wait() })
			lines := bufio.NewScanner(stdout)
			require.True(lines.Scan(), "child must enter command")
			require.Equal("ready", lines.Text())
			// This polls a real inherited pipe; timing bounds a kernel transition.
			require.Eventually(func() bool {
				n, probeErr := unix.IoctlGetInt(int(input.Fd()), unix.TIOCINQ)
				return probeErr == nil && n == 0
			}, 5*time.Second, 10*time.Millisecond)
			if mode == "eof" {
				require.NoError(writer.Close())
			} else {
				_, err = release.Write([]byte{1})
				require.NoError(err)
			}
			require.NoError(child.Wait(), "command must exit while owner writer remains open")
			assert.NoError(ctx.Err(), "deadline must not release the blocked child")
		})
	}
}

func TestBackupOwnerInheritedStdinChild(t *testing.T) {
	mode := os.Getenv("MSGVAULT_TEST_OWNER_STDIN")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require := require.New(t)
	assert := assert.New(t)
	command := &cobra.Command{}
	command.SetContext(ctx)
	command.SetIn(os.Stdin)
	command.Flags().Bool(backupOwnerInputFlag, true, "")
	failure := errors.New("command failed")
	err := withBackupOwnerInput(command, func() error {
		_, writeErr := fmt.Fprintln(os.Stdout, "ready")
		require.NoError(writeErr)
		if mode == "eof" {
			<-command.Context().Done()
			return command.Context().Err()
		}
		control := os.NewFile(3, "release")
		defer func() { _ = control.Close() }()
		var signal [1]byte
		_, readErr := control.Read(signal[:])
		require.NoError(readErr)
		switch mode {
		case "command-failure":
			return failure
		case "parent-cancel":
			cancel()
			<-command.Context().Done()
			return command.Context().Err()
		}
		return nil
	})
	switch mode {
	case "success":
		require.NoError(err)
	case "command-failure":
		require.ErrorIs(err, failure)
	case "parent-cancel":
		require.ErrorIs(err, context.Canceled)
	case "eof":
		require.ErrorIs(err, errBackupOwnerInputClosed)
	}
	assert.Equal(ctx, command.Context())
}

func TestBackupOwnerInputPreservesDescriptorFlags(t *testing.T) {
	require := require.New(t)
	input, writer, err := os.Pipe()
	require.NoError(err)
	defer func() { _ = writer.Close() }()
	raw, err := input.SyscallConn()
	require.NoError(err)
	duplicate := -1
	require.NoError(raw.Control(func(fd uintptr) {
		require.NoError(unix.SetNonblock(int(fd), true))
		duplicate, err = unix.Dup(int(fd))
	}))
	require.NoError(err)
	defer func() { _ = unix.Close(duplicate) }()
	before, err := unix.FcntlInt(uintptr(duplicate), unix.F_GETFL, 0)
	require.NoError(err)
	require.NotZero(before & unix.O_NONBLOCK)
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetIn(input)
	command.Flags().Bool(backupOwnerInputFlag, true, "")
	require.NoError(withBackupOwnerInput(command, func() error { return nil }))
	after, err := unix.FcntlInt(uintptr(duplicate), unix.F_GETFL, 0)
	require.NoError(err)
	require.Equal(before, after, "stdin observation must not change another descriptor's flags")
}

func TestBackupOwnerSocketInput(t *testing.T) {
	for _, socketType := range []int{unix.SOCK_STREAM, unix.SOCK_DGRAM} {
		for _, nonblocking := range []bool{false, true} {
			t.Run(fmt.Sprint(socketType, "-", nonblocking), func(t *testing.T) {
				require := require.New(t)
				kind := socketType | unix.SOCK_CLOEXEC
				if nonblocking {
					kind |= unix.SOCK_NONBLOCK
				}
				pair, err := unix.Socketpair(unix.AF_UNIX, kind, 0)
				require.NoError(err)
				input := os.NewFile(uintptr(pair[0]), "owner-socket")
				defer func() { _ = unix.Close(pair[1]) }()
				command := &cobra.Command{}
				command.SetContext(t.Context())
				command.SetIn(input)
				command.Flags().Bool(backupOwnerInputFlag, true, "")
				called := false
				err = withBackupOwnerInput(command, func() error { called = true; return nil })
				if socketType != unix.SOCK_STREAM {
					require.ErrorContains(err, "must be a stream")
					require.False(called)
				} else if nonblocking {
					require.NoError(err)
					require.True(called)
				} else {
					require.ErrorContains(err, "must be nonblocking")
					require.False(called, "invalid input must refuse before starting work")
				}
				_, err = input.Stat()
				require.ErrorIs(err, os.ErrClosed)
			})
		}
	}
}
