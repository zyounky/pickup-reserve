package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zyounky/pickup-reserve/internal/httpapi"
	"github.com/zyounky/pickup-reserve/internal/stock"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	data := flag.String("data", "data/stock.json", "snapshot path (empty for in-memory storage)")
	flag.Parse()

	if err := run(*addr, *data); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(addr, data string) error {
	store, err := stock.New(data, time.Now)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           httpapi.New(store),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       time.Minute,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	slog.Info("listening", "addr", listener.Addr().String(), "snapshot", data)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		return err
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
