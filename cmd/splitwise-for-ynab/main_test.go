package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	runInterval = 5 * time.Millisecond
	runTimeout  = 2 * time.Second
)

func TestRun(t *testing.T) {
	t.Run("calls fn immediately then on each tick", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := make(chan struct{}, 10)
		go run(ctx, runInterval, func(context.Context) error {
			calls <- struct{}{}
			return nil
		})
		for range 3 {
			select {
			case <-calls:
			case <-time.After(runTimeout):
				t.Fatal("fn not called within timeout")
			}
		}
	})

	t.Run("continues after error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := make(chan struct{}, 10)
		go run(ctx, runInterval, func(context.Context) error {
			calls <- struct{}{}
			return errors.New("simulated error")
		})
		for range 3 {
			select {
			case <-calls:
			case <-time.After(runTimeout):
				t.Fatal("fn not called 3 times after errors within timeout")
			}
		}
	})

	t.Run("stops on cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() {
			run(ctx, runInterval, func(context.Context) error { return nil })
			close(stopped)
		}()
		cancel()
		select {
		case <-stopped:
		case <-time.After(runTimeout):
			t.Fatal("run did not stop after context cancel")
		}
	})
}

func TestListenAddr(t *testing.T) {
	for _, tt := range []struct {
		port, listenAddr, want string
	}{
		{"8080", "", ":8080"},
		{"9000", "", ":9000"},
		{"8080", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"8080", "127.0.0.1:9000", "127.0.0.1:9000"},
	} {
		cfg := &config{Port: tt.port, ListenAddr: tt.listenAddr}
		if got := cfg.listenAddr(); got != tt.want {
			t.Errorf("listenAddr() with PORT=%q LISTEN_ADDR=%q = %q, want %q",
				tt.port, tt.listenAddr, got, tt.want)
		}
	}
}
