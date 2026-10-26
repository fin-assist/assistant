package postgres_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"

	"github.com/fin-assist/assistant/internal/adapter/postgres"
)

// go-postgres v0.3.0 (FIN-277): a caller context that has already ended is reported as
// itself, never as an unavailable database; nothing is dialled for it.
func TestUnitOpenWithEndedContextReportsTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := postgres.Open(ctx, postgres.Options{PrimaryDSN: "postgres://u:secret@127.0.0.1:1/db?connect_timeout=1"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, pgstore.ErrPoolUnavailable) {
		t.Fatalf("ended caller context: %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error %v must not contain the password", err)
	}
}

// silentPrimary accepts TCP connections and never answers, like a hung primary.
func silentPrimary(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return lis.Addr().String()
}

// go-postgres v0.3.0 (FIN-277): the driver's connect_timeout against a primary that never
// answers, with the caller context still live, is ErrPoolUnavailable — not
// context.DeadlineExceeded as in v0.2.0, which a caller could mistake for its own deadline.
func TestIntegrationOpenSilentPrimaryIsPoolUnavailable(t *testing.T) {
	addr := silentPrimary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	_, err := postgres.Open(ctx, postgres.Options{PrimaryDSN: "postgres://u:secret@" + addr + "/db?sslmode=disable&connect_timeout=1"})
	if !errors.Is(err, pgstore.ErrPoolUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("silent primary with a live context: %v", err)
	}
	if ctx.Err() != nil || time.Since(started) > 20*time.Second {
		t.Fatalf("open was bounded by the caller instead of connect_timeout: %v after %s", ctx.Err(), time.Since(started))
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), addr) {
		t.Fatalf("error %v must not contain the DSN", err)
	}
}
