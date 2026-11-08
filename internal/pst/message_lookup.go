package pst

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
)

// GetMessage retrieves one normal message by its PST node identifier.
func (f *File) GetMessage(identifier int64) (*pstlib.Message, error) {
	message, err := f.pstFile.GetMessage(pstlib.Identifier(identifier))
	if err != nil {
		return nil, fmt.Errorf("read PST message %d: %w", identifier, err)
	}
	return message, nil
}

// Stat reports information about the open source file, without reopening it.
func (f *File) Stat() (os.FileInfo, error) {
	file, ok := f.closer.(*os.File)
	if !ok {
		return nil, errors.New("PST source is not a file")
	}
	return file.Stat()
}

// ArchiveFingerprint matches the importer's fingerprint on this open file,
// ensuring a pathname change cannot redirect the subsequent entry lookup.
func (f *File) ArchiveFingerprint() (string, error) {
	r, ok := f.closer.(io.ReaderAt)
	if !ok {
		return "", errors.New("PST file does not support random access")
	}
	var buf [4096]byte
	n, err := r.ReadAt(buf[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	sum := sha256.Sum256(buf[:n])
	return hex.EncodeToString(sum[:6]), nil
}
