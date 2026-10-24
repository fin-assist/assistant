package aistudio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIntegrationFileCredentialRotationAndRefusalBeforeProviderHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PRIVATE-credential-path")
	replace := func(key string) {
		t.Helper()
		next := path + ".next"
		if err := os.WriteFile(next, []byte(key), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, path); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	headers := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		headers <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(httpTestSSE("saved model response")))
	}))
	t.Cleanup(server.Close)
	c := httpTestClient(t, server, func(o *Options) { o.Key = FileKey{Path: path} })
	assertKey := func(want string) {
		t.Helper()
		var output strings.Builder
		if _, err := c.Stream(httpTestContext(t), httpTestRequest(), func(delta Delta) error { output.WriteString(delta.Text); return nil }); err != nil {
			t.Fatal(err)
		}
		if got := <-headers; got != "Bearer "+want || output.String() != "saved model response" {
			t.Fatalf("provider key=%q output=%q", got, output.String())
		}
	}
	replace(" first-provider-key\n")
	assertKey("first-provider-key")
	replace("second-provider-key-after-rotation")
	assertKey("second-provider-key-after-rotation")
	for _, state := range []string{"empty", "newline-in-header", "oversized", "missing", "directory"} {
		t.Run(state, func(t *testing.T) {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "empty":
				replace(" \n")
			case "newline-in-header":
				replace("PRIVATE\r\nInjected: value")
			case "oversized":
				replace(strings.Repeat("x", 8193))
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			before := calls.Load()
			_, err := c.Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { t.Error("invalid credential emitted model text"); return nil })
			if err == nil || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), path) {
				t.Fatalf("credential error leaked path/key: %v", err)
			}
			if calls.Load() != before {
				t.Fatal("invalid credential reached HTTPprovider")
			}
		})
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replace("recovered-provider-key")
	assertKey("recovered-provider-key")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := calls.Load()
	if _, err := c.Stream(ctx, httpTestRequest(), func(Delta) error { return nil }); err == nil {
		t.Fatal("cancelled request accepted")
	}
	if calls.Load() != before {
		t.Fatal("cancelled request reachedprovider")
	}
	if calls.Load() != 3 {
		t.Fatalf("provider calls=%d want3", calls.Load())
	}
}
