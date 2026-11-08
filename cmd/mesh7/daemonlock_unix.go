//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// heldLock keeps the locked file reachable. An *os.File nobody references is
// closed by its finalizer at the next garbage collection, and closing the
// descriptor releases the flock: without this variable the daemon lost its
// lock a few minutes after starting.
var heldLock *os.File

// lockFile takes an exclusive advisory lock on path and keeps the file open,
// so the lock lasts as long as the process. It reports false, nil when
// another process holds it.
func lockFile(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	// Deliberately never closed: the kernel releases the lock when the
	// process exits, however it exits.
	heldLock = f
	return true, nil
}

// lockHeld reports whether another process holds the lock on path, without
// keeping it.
func lockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false // no file: no daemon ever started for this config
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
