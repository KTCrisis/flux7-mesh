package trace

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"
)

// Integrity envelope.
//
// Every line the store writes ends with four keys appended after the entry's
// own JSON:
//
//	{...entry...,"seq":42,"alg":"hmac-sha256","prev_hash":"ab..","hash":"cd.."}
//
// hash = MAC(key, "<seq>\n<alg>\n<prev_hash>\n" + <entry JSON as written>)
//
// The entry JSON is hashed as the exact bytes on disk, never re-marshalled:
// a float or a map re-encoded after a round trip may not come back identical.
// seq and alg are inside the MAC, so a line cannot be renumbered, and a chain
// cannot be downgraded from HMAC to plain SHA-256 without the key.
//
// With a key (MESH_TRACE_KEY), rewriting the chain requires the key. Without
// one the chain is plain SHA-256: it catches an edited or deleted line in the
// middle, not someone who recomputes every hash after it. Neither detects a
// truncated tail on its own; the head hash is logged at startup and shutdown
// so an external log holds a point to compare against.

const (
	AlgSHA256     = "sha256"
	AlgHMACSHA256 = "hmac-sha256"
)

// envelopeMarker starts the envelope. Inside the entry JSON a string value
// cannot contain an unescaped quote, so the last occurrence of this marker is
// always the envelope, even if params carry a "seq" key.
var envelopeMarker = []byte(`,"seq":`)

type envelope struct {
	Seq      uint64 `json:"seq"`
	Alg      string `json:"alg"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

func chainHash(key []byte, alg string, seq uint64, prev string, payload []byte) string {
	var h hash.Hash
	if alg == AlgHMACSHA256 {
		h = hmac.New(sha256.New, key)
	} else {
		h = sha256.New()
	}
	h.Write([]byte(strconv.FormatUint(seq, 10) + "\n" + alg + "\n" + prev + "\n"))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// sealLine appends the envelope to an entry's JSON.
func sealLine(payload []byte, env envelope) []byte {
	var b bytes.Buffer
	b.Write(payload[:len(payload)-1]) // drop the closing brace
	fmt.Fprintf(&b, `,"seq":%d,"alg":%q,"prev_hash":%q,"hash":%q}`, env.Seq, env.Alg, env.PrevHash, env.Hash)
	return b.Bytes()
}

// splitLine separates the entry JSON from its envelope. ok is false for a
// line written before the chain existed.
func splitLine(line []byte) (payload []byte, env envelope, ok bool) {
	i := bytes.LastIndex(line, envelopeMarker)
	if i < 0 {
		return line, env, false
	}
	if err := json.Unmarshal(append([]byte("{"), line[i+1:]...), &env); err != nil || env.Hash == "" {
		return line, env, false
	}
	payload = make([]byte, 0, i+1)
	payload = append(payload, line[:i]...)
	payload = append(payload, '}')
	return payload, env, true
}

// Break describes the first line where the chain does not hold.
type Break struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Seq    uint64 `json:"seq,omitempty"`
	Reason string `json:"reason"`
}

// Report is the outcome of Verify.
type Report struct {
	Lines     int    `json:"lines"`
	Unchained int    `json:"unchained"` // legacy lines written before the chain started
	Chained   int    `json:"chained"`
	FirstSeq  uint64 `json:"first_seq,omitempty"`
	LastSeq   uint64 `json:"last_seq,omitempty"`
	Anchor    string `json:"anchor,omitempty"` // prev_hash of the first chained line
	Head      string `json:"head,omitempty"`   // hash of the last chained line
	Alg       string `json:"alg,omitempty"`
	Break     *Break `json:"break,omitempty"`
}

// Verify walks one or more trace files in order (oldest first, e.g.
// traces.jsonl.old then traces.jsonl) and stops at the first break. With a
// key, every chained line must be HMAC: a plain SHA-256 line is a downgrade.
// Without a key, HMAC lines cannot be checked and count as a break.
func Verify(paths []string, key []byte) (Report, error) {
	var r Report
	var prev string
	started := false
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return r, err
		}
		err = verifyReader(f, p, key, &r, &prev, &started)
		f.Close()
		if err != nil {
			return r, err
		}
		if r.Break != nil {
			return r, nil
		}
	}
	return r, nil
}

func verifyReader(rd io.Reader, file string, key []byte, r *Report, prev *string, started *bool) error {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		r.Lines++
		payload, env, ok := splitLine(line)
		fail := func(reason string) {
			r.Break = &Break{File: file, Line: n, Seq: env.Seq, Reason: reason}
		}
		if !ok {
			if *started {
				fail("unchained line inside the chain")
				return nil
			}
			r.Unchained++
			continue
		}
		switch {
		case key != nil && env.Alg != AlgHMACSHA256:
			fail("alg " + env.Alg + " where hmac-sha256 is required")
			return nil
		case key == nil && env.Alg == AlgHMACSHA256:
			fail("hmac-sha256 line and no key to check it (set MESH_TRACE_KEY)")
			return nil
		case env.Alg != AlgHMACSHA256 && env.Alg != AlgSHA256:
			fail("unknown alg " + env.Alg)
			return nil
		}
		if *started {
			if env.Seq != r.LastSeq+1 {
				fail(fmt.Sprintf("seq %d follows %d", env.Seq, r.LastSeq))
				return nil
			}
			if env.PrevHash != *prev {
				fail("prev_hash does not match the previous line")
				return nil
			}
		}
		want := chainHash(key, env.Alg, env.Seq, env.PrevHash, payload)
		if !hmac.Equal([]byte(want), []byte(env.Hash)) {
			fail("hash does not match the content")
			return nil
		}
		if !*started {
			*started = true
			r.FirstSeq = env.Seq
			r.Anchor = env.PrevHash
		}
		r.Chained++
		r.LastSeq = env.Seq
		r.Head = env.Hash
		r.Alg = env.Alg
		*prev = env.Hash
	}
	return sc.Err()
}

// lastEnvelope returns the envelope of the last chained line of a file.
func lastEnvelope(path string) (envelope, bool) {
	f, err := os.Open(path)
	if err != nil {
		return envelope{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var last envelope
	found := false
	for sc.Scan() {
		if _, env, ok := splitLine(sc.Bytes()); ok {
			last, found = env, true
		}
	}
	return last, found
}
