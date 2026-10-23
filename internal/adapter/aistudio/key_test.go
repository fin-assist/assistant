package aistudio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileKeyRotationAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	source := FileKey{Path: path}
	if _, err := source.Key(t.Context()); err == nil || strings.Contains(err.Error(), path) {
		t.Fatal("missing credential error leaked path")
	}
	for _, v := range []string{"first-key\n", "second-key\n"} {
		tmp := filepath.Join(dir, "next")
		if err := os.WriteFile(tmp, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		got, err := source.Key(t.Context())
		if err != nil || got != strings.TrimSpace(v) {
			t.Fatalf("rotation %q %v", got, err)
		}
	}
	for _, v := range []string{" ", "bad\r\ninjected", "bad\tkey", strings.Repeat("x", 8193), "ключ"} {
		if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := source.Key(t.Context()); err == nil || got != "" || len(strings.TrimSpace(v)) > 3 && strings.Contains(err.Error(), v) {
			t.Fatal("invalid key accepted or leaked")
		}
	}
	if _, err := (FileKey{Path: dir}).Key(t.Context()); err == nil {
		t.Fatal("directory accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.Key(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
