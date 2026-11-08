package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The restart race. A `mesh7 --mcp` client that finds no daemon on the port
// runs standalone and serves its own approval API on that port. A daemon
// spends a few seconds initialising (upstream servers, catalogue) before it
// listens, so a client started in that window took the port, and the daemon
// then failed with "address already in use" and restarted in a loop.
//
// The daemon now holds a lock file for its whole life, taken before
// initialisation. A client that finds no daemon on the port checks the lock:
// held means a daemon is starting, so the client waits for it instead of
// taking its place. No daemon, no lock: the client runs standalone as before.

// daemonWaitDefault bounds how long a client waits for a starting daemon.
const daemonWaitDefault = 30 * time.Second

// daemonLockPath is the lock file of the daemon serving this config, in the
// user cache directory (the daemon and its clients run as the same user).
func daemonLockPath(configPath string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "mesh7", configID(configPath)+".daemon.lock")
}

// holdDaemonLock takes the daemon lock for this config and keeps it until the
// process exits. A failure is logged, never fatal: without the lock the only
// loss is the protection against the restart race.
func holdDaemonLock(configPath string) {
	path := daemonLockPath(configPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("daemon lock: cannot create its directory", "path", path, "error", err)
		return
	}
	// A client probes the lock for an instant; retry briefly rather than give
	// up because one was looking at that moment.
	for deadline := time.Now().Add(2 * time.Second); ; {
		held, err := lockFile(path)
		if err == nil && held {
			return
		}
		if err != nil {
			slog.Warn("daemon lock: unavailable", "path", path, "error", err)
			return
		}
		if time.Now().After(deadline) {
			slog.Warn("daemon lock held by another process: another daemon may serve this config", "path", path)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// daemonStarting reports whether a daemon for this config holds its lock.
func daemonStarting(configPath string) bool {
	return lockHeld(daemonLockPath(configPath))
}

// waitForDaemon polls the port until a daemon answers, up to max. It returns
// what daemonOnPort returns once one does, or false at the deadline.
func waitForDaemon(port int, max time.Duration) (bool, string) {
	for deadline := time.Now().Add(max); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if running, theirs := daemonOnPort(port); running {
			return true, theirs
		}
	}
	return false, ""
}

// daemonWait is how long a client waits for a starting daemon:
// MESH7_DAEMON_WAIT in seconds, else daemonWaitDefault.
func daemonWait() time.Duration {
	if s, err := strconv.Atoi(os.Getenv("MESH7_DAEMON_WAIT")); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	return daemonWaitDefault
}
