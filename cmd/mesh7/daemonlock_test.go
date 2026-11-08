//go:build unix

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A held daemon lock is seen by a client probe, and the probe does not take
// it away.
func TestDaemonLockSeenByClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.lock")
	if lockHeld(path) {
		t.Fatal("no daemon yet, lock reported held")
	}
	if held, err := lockFile(path); err != nil || !held {
		t.Fatalf("daemon could not take its lock: %v %v", held, err)
	}
	if !lockHeld(path) {
		t.Fatal("client does not see the daemon's lock")
	}
	if !lockHeld(path) {
		t.Fatal("a client probe released the daemon's lock")
	}
	// The lock must survive garbage collection: an unreferenced *os.File is
	// closed by its finalizer, which releases the flock.
	runtime.GC()
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	if !lockHeld(path) {
		t.Fatal("the daemon's lock was released by a garbage collection")
	}
	if held, _ := lockFile(path); held {
		t.Fatal("a second daemon took a held lock")
	}
}

// A client waits for a daemon that starts listening a little later.
func TestWaitForDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens yet: the daemon is "initialising"

	srv := &httptest.Server{Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","config":"abc"}`))
	})}}
	go func() {
		time.Sleep(600 * time.Millisecond)
		l, err := net.Listen("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		srv.Listener = l
		srv.Start()
	}()
	defer func() {
		if srv.Listener != nil {
			srv.Close()
		}
	}()

	running, theirs := waitForDaemon(port, 5*time.Second)
	if !running || theirs != "abc" {
		t.Fatalf("waitForDaemon = %v, %q; want the daemon once it listens", running, theirs)
	}
	if running, _ := waitForDaemon(port+1, 300*time.Millisecond); running {
		t.Fatal("waitForDaemon found a daemon on a silent port")
	}
}
