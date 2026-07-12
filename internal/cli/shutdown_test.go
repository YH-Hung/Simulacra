package cli

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestShutdownWaiterReturnsWhenContextCanceledWithoutSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		waitAndShutdownContext(ctx, make(chan os.Signal), time.Minute, func() {}, func() {}, func(...any) {})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown waiter did not return after context cancellation")
	}
}

func TestShutdownGracefulCompletes(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var forced atomic.Bool
	sig <- syscall.SIGINT
	waitAndShutdown(sig, time.Second,
		func() {}, // graceful returns immediately
		func() { forced.Store(true) },
		func(...any) {})
	if forced.Load() {
		t.Error("force called even though graceful stop completed")
	}
}

func TestShutdownSecondSignalForces(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var forced atomic.Bool
	blocked := make(chan struct{}) // never closed: graceful hangs forever
	sig <- syscall.SIGINT
	sig <- syscall.SIGINT // second Ctrl-C already queued
	done := make(chan struct{})
	go func() {
		waitAndShutdown(sig, time.Minute,
			func() { <-blocked },
			func() { forced.Store(true) },
			func(...any) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitAndShutdown did not return after second signal")
	}
	if !forced.Load() {
		t.Error("second signal did not force stop")
	}
}

func TestShutdownTimeoutForces(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var forced atomic.Bool
	blocked := make(chan struct{})
	sig <- syscall.SIGINT
	done := make(chan struct{})
	go func() {
		waitAndShutdown(sig, 50*time.Millisecond,
			func() { <-blocked },
			func() { forced.Store(true) },
			func(...any) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitAndShutdown did not return after timeout")
	}
	if !forced.Load() {
		t.Error("timeout did not force stop")
	}
}
