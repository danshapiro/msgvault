//go:build unix

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func backupOwnerInputReader(ctx context.Context, input io.ReadCloser, closeInput func()) (func() error, func(), error) {
	file, ok := input.(*os.File)
	if !ok {
		read, interrupt := backupOwnerCloseReader(input, closeInput)
		return read, interrupt, nil
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat backup owner input: %w", err)
	}
	if info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return nil, nil, errors.New("backup owner input must be a pipe or socket")
	}
	// This input is exclusively owned by the command. Poll before each bounded
	// read, and keep the descriptor open until the reader has joined. No other
	// reader can drain readiness, and cleanup never races Close or fd reuse.
	// In particular, do not alter inherited O_NONBLOCK flags shared with a caller.
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, nil, fmt.Errorf("access backup owner input descriptor: %w", err)
	}
	// A socket can report spurious readiness. Its existing nonblocking flag
	// must make a subsequent read cancellable; never change shared flags.
	if info.Mode()&os.ModeSocket != 0 {
		var flags, socketType int
		var flagErr, typeErr error
		if err := raw.Control(func(fd uintptr) {
			flags, flagErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
			socketType, typeErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
		}); err != nil {
			return nil, nil, fmt.Errorf("inspect backup owner socket: %w", err)
		}
		if flagErr != nil {
			return nil, nil, fmt.Errorf("read backup owner socket flags: %w", flagErr)
		}
		if typeErr != nil {
			return nil, nil, fmt.Errorf("read backup owner socket type: %w", typeErr)
		}
		if socketType != unix.SOCK_STREAM {
			return nil, nil, errors.New("backup owner socket input must be a stream")
		}
		if flags&unix.O_NONBLOCK == 0 {
			return nil, nil, errors.New("backup owner socket input must be nonblocking")
		}
	}
	return func() error {
		var readErr error
		if err := raw.Control(func(fd uintptr) {
			readErr = readBackupOwnerDescriptor(ctx, int(fd))
		}); err != nil {
			return fmt.Errorf("control backup owner input descriptor: %w", err)
		}
		return readErr
	}, func() {}, nil
}

// The descriptor stays valid for the entire RawConn.Control callback. Unlike
// File.Fd, this neither switches a pollable file to blocking mode nor exposes
// a descriptor outside its guaranteed lifetime.
func readBackupOwnerDescriptor(ctx context.Context, fd int) error {
	const maximumPollFD = 1<<31 - 1
	if fd < 0 || fd > maximumPollFD {
		return errors.New("backup owner input descriptor is outside poll range")
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("backup owner input canceled: %w", err)
		}
		_, err := unix.Poll(poll, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll backup owner input: %w", err)
		}
		if poll[0].Revents == 0 {
			continue
		}
		n, err := unix.Read(fd, buffer)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			continue
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read backup owner input: %w", err)
		}
		if n == 0 {
			return nil
		}
	}
}
