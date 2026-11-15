//go:build !unix

package cmd

import (
	"context"
	"io"
)

func backupOwnerInputReader(_ context.Context, input io.ReadCloser, closeInput func()) (func() error, func(), error) {
	read, interrupt := backupOwnerCloseReader(input, closeInput)
	return read, interrupt, nil
}
