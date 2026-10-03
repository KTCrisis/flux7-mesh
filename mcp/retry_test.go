package mcp

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryConnectUntilItWorks(t *testing.T) {
	var calls, fails atomic.Int32
	ok := RetryConnect(make(chan struct{}), "memory", func() error {
		if calls.Add(1) < 3 {
			return errors.New("connection refused")
		}
		return nil
	}, time.Millisecond, 4*time.Millisecond, func(error) { fails.Add(1) })
	if !ok || calls.Load() != 3 || fails.Load() != 2 {
		t.Fatalf("ok=%v calls=%d fails=%d", ok, calls.Load(), fails.Load())
	}
}

func TestRetryConnectStops(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan bool)
	go func() {
		done <- RetryConnect(stop, "memory", func() error { return errors.New("down") }, time.Millisecond, time.Millisecond, nil)
	}()
	time.Sleep(10 * time.Millisecond)
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Error("a stopped retry does not report success")
		}
	case <-time.After(time.Second):
		t.Fatal("RetryConnect did not stop")
	}
}

func TestManagerShowsRetryingUpstreams(t *testing.T) {
	m := NewManager()
	m.SetRetrying("memory", "streamable-http", errors.New("connection refused"))
	st := m.ServerStatuses().([]ServerStatus)
	if len(st) != 1 || st[0].Status != "retrying" || st[0].Error != "connection refused" {
		t.Fatalf("statuses: %+v", st)
	}
	m.Add(&MCPClient{Name: "memory", Transport: "streamable-http", status: "ready", done: newDoneChan()})
	for _, s := range m.ServerStatuses().([]ServerStatus) {
		if s.Status == "retrying" {
			t.Errorf("a connected upstream is no longer retrying: %+v", s)
		}
	}
}
