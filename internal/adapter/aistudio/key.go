package aistudio

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
)

// FileKey reads a protected API/IAM key file for each model step. Atomic file
// replacement is observed without a restart; neither errors nor logs include its path.
type FileKey struct{ Path string }

// Key reads a bounded, nonempty header value, preserving context cancellation.
func (f FileKey) Key(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return "", errors.New("AI Studio credential unavailable")
	}
	defer func() { _ = file.Close() }()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("invalid AI Studio credential file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return "", contextOr(ctx, "AI Studio credential unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(data))
	if len(data) > 8192 || !validKey(key) {
		return "", errors.New("invalid AI Studio credential")
	}
	return key, nil
}
