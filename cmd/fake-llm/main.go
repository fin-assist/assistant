// fake-llm is a local-only scenario server. It is absent from the service image.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fin-assist/assistant/internal/fakellm"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("fake-llm", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", "127.0.0.1:8092", "loopback listen address")
	file := flags.String("scenarios", "", "JSON suite; empty uses embedded scenarios")
	if err := flags.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || flags.NArg() != 0 {
		return errors.New("fake-llm requires a loopback IP listen address")
	}
	handler, err := fakellm.Default()
	if *file != "" {
		f, e := os.Open(*file)
		if e != nil {
			return errors.New("cannot open scenarios")
		}
		defer func() { _ = f.Close() }()
		handler, err = fakellm.Load(f)
	}
	if err != nil {
		return err
	}
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		return errors.New("cannot listen for fake model")
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 4 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384, BaseContext: func(net.Listener) context.Context { return ctx }}
	if _, err := fmt.Fprintln(stderr, "fake model listening at", lis.Addr()); err != nil {
		_ = lis.Close()
		return errors.New("cannot report fake model listener")
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(lis) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := srv.Shutdown(shutdown); e != nil {
			_ = srv.Close()
		}
		err = <-done
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
