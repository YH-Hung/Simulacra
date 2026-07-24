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
	gracefulStarted := make(chan struct{})
	gracefulReleased := make(chan struct{})
	forced := make(chan struct{})
	done := make(chan struct{})
	go func() {
		waitAndShutdownContext(ctx, make(chan os.Signal), time.Minute, func() {
			close(gracefulStarted)
			<-gracefulReleased
		}, func() {
			close(forced)
			close(gracefulReleased)
		}, func(...any) {})
		close(done)
	}()
	cancel()
	select {
	case <-gracefulStarted:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not start graceful shutdown")
	}
	select {
	case <-forced:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not force a blocked graceful shutdown")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown waiter did not return after context cancellation")
	}
}

func TestShutdownContextCanceledDuringGraceForcesAndJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	gracefulStarted := make(chan struct{})
	gracefulReleased := make(chan struct{})
	forceReturned := make(chan struct{})
	done := make(chan struct{})
	go func() {
		waitAndShutdownContext(ctx, sig, time.Minute, func() {
			close(gracefulStarted)
			<-gracefulReleased
		}, func() {
			close(gracefulReleased)
			close(forceReturned)
		}, func(...any) {})
		close(done)
	}()
	sig <- syscall.SIGINT
	<-gracefulStarted
	cancel()
	<-forceReturned
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown waiter did not join graceful shutdown after forcing")
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

func TestShutdownWaiterStopDoesNotForceGraceAlreadyInProgress(t *testing.T) {
	stopWaiter := make(chan struct{})
	sig := make(chan os.Signal, 1)
	gracefulStarted := make(chan struct{})
	releaseGraceful := make(chan struct{})
	var forced atomic.Bool
	done := make(chan struct{})
	go func() {
		waitAndShutdownContextStop(context.Background(), stopWaiter, sig, time.Second, func() {
			close(gracefulStarted)
			<-releaseGraceful
		}, func() { forced.Store(true) }, func(...any) {})
		close(done)
	}()
	sig <- syscall.SIGINT
	<-gracefulStarted
	close(stopWaiter) // Serve returns when GracefulStop closes its listener.
	select {
	case <-done:
		t.Fatal("idle-waiter stop ended an in-progress graceful shutdown")
	case <-time.After(50 * time.Millisecond):
	}
	if forced.Load() {
		t.Fatal("idle-waiter stop converted first signal into force")
	}
	close(releaseGraceful)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after graceful drain completed")
	}
}

func TestShutdownSecondSignalForces(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var forced atomic.Bool
	blocked := make(chan struct{})
	sig <- syscall.SIGINT
	sig <- syscall.SIGINT // second Ctrl-C already queued
	done := make(chan struct{})
	go func() {
		waitAndShutdown(sig, time.Minute,
			func() { <-blocked },
			func() {
				forced.Store(true)
				close(blocked)
			},
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
			func() {
				forced.Store(true)
				close(blocked)
			},
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
