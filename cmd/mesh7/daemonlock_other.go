//go:build !unix

package main

// Without flock the restart race stays possible on this platform: the daemon
// takes no lock and a client never waits.

func lockFile(path string) (bool, error) { return true, nil }

func lockHeld(path string) bool { return false }
