package trace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type collector struct {
	mu     sync.Mutex
	posts  int
	spans  int
	status func(post int) int
}

func (c *collector) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body otlpExport
		json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.posts++
		n := c.posts
		c.mu.Unlock()
		code := http.StatusOK
		if c.status != nil {
			code = c.status(n)
		}
		if code < 300 {
			c.mu.Lock()
			c.spans += len(body.ResourceSpans[0].ScopeSpans[0].Spans)
			c.mu.Unlock()
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func entryN(i int) Entry {
	return Entry{TraceID: NewID(), Timestamp: time.Now(), Tool: "t", LatencyMs: int64(i)}
}

// Spans leave in batches, and Close sends what is still queued.
func TestOTLPBatchesAndFlushesOnClose(t *testing.T) {
	c := &collector{}
	exp := NewOTELExporter(c.server(t).URL)
	for i := 0; i < otelBatchSize+10; i++ {
		exp.Export(entryN(i))
	}
	exp.Close()
	if c.spans != otelBatchSize+10 || c.posts != 2 {
		t.Fatalf("collector got %d spans in %d posts, want %d in 2", c.spans, c.posts, otelBatchSize+10)
	}
	if st := exp.Stats(); st.Sent != int64(otelBatchSize+10) || st.Failed != 0 || st.Dropped != 0 {
		t.Errorf("stats = %+v", st)
	}
}

// A collector that answers 503 twice gets the same batch a third time.
func TestOTLPRetriesTransientFailures(t *testing.T) {
	c := &collector{status: func(n int) int {
		if n <= 2 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}}
	exp := NewOTELExporter(c.server(t).URL)
	exp.backoff = time.Millisecond
	exp.Export(entryN(1))
	exp.send([]otlpSpan{exp.toOTLP(entryN(2)).ResourceSpans[0].ScopeSpans[0].Spans[0]})
	if c.posts != 3 || exp.Stats().Sent != 1 {
		t.Fatalf("posts = %d, stats = %+v", c.posts, exp.Stats())
	}
	exp.Close()
}

// A 400 is final: no retry, counted as failed.
func TestOTLPDoesNotRetryRejection(t *testing.T) {
	c := &collector{status: func(int) int { return http.StatusBadRequest }}
	exp := NewOTELExporter(c.server(t).URL)
	exp.backoff = time.Millisecond
	exp.send([]otlpSpan{exp.toOTLP(entryN(1)).ResourceSpans[0].ScopeSpans[0].Spans[0]})
	if c.posts != 1 || exp.Stats().Failed != 1 {
		t.Fatalf("posts = %d, stats = %+v", c.posts, exp.Stats())
	}
	exp.Close()
}

// A full queue drops the span instead of blocking the caller.
func TestOTLPFullQueueDropsWithoutBlocking(t *testing.T) {
	exp := &OTELExporter{endpoint: "http://unused", service: "t", queue: make(chan otlpSpan, 1)}
	var returned atomic.Bool
	go func() {
		exp.Export(entryN(1))
		exp.Export(entryN(2))
		returned.Store(true)
	}()
	deadline := time.Now().Add(time.Second)
	for !returned.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !returned.Load() {
		t.Fatal("Export blocked on a full queue")
	}
	if exp.Stats().Dropped != 1 {
		t.Errorf("dropped = %d, want 1", exp.Stats().Dropped)
	}
}
