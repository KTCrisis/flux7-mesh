package mcp

import (
	"log/slog"
	"time"
)

// RetryConnect calls connect until it succeeds or stop closes, waiting
// first, then twice as long after each failure, up to max. It is how an
// upstream that was down when mesh7 started still joins later: without
// it, a server unreachable at that moment never came back.
func RetryConnect(stop <-chan struct{}, name string, connect func() error, first, max time.Duration, onFail func(error)) bool {
	wait := first
	for {
		select {
		case <-stop:
			return false
		case <-time.After(wait):
		}
		err := connect()
		if err == nil {
			slog.Info("MCP upstream connected after retry", "name", name)
			return true
		}
		if onFail != nil {
			onFail(err)
		}
		wait = min(wait*2, max)
		slog.Warn("MCP upstream still unreachable", "name", name, "error", err, "next_try", wait)
	}
}
