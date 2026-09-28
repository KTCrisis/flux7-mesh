// Package pin keeps a fingerprint of every tool an upstream MCP server
// declares, and holds back tools that appear or change after the operator
// last accepted the catalogue.
//
// Trust is given on first use: the first time a server is seen, its whole
// catalogue is pinned as is. From then on a tool the server adds is denied,
// and a tool whose description, schema or annotations changed asks for human
// approval, until the operator accepts the new version. This answers the
// "rug pull": a tool reviewed on day 1 and rewritten on day 7.
package pin

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/KTCrisis/flux7-mesh/registry"
)

// Status of a tool against its pin.
type Status string

const (
	Pinned  Status = "pinned"  // matches the accepted fingerprint
	New     Status = "new"     // the server did not declare it when pinned
	Changed Status = "changed" // declared before, different now
)

// Floor is the least permissive action a status tolerates.
func (s Status) Floor() string {
	switch s {
	case New:
		return "deny"
	case Changed:
		return "human_approval"
	}
	return ""
}

type entry struct {
	Fingerprint string
	Description string
	PinnedAt    time.Time
}

// View is one tool as the control plane reports it.
type View struct {
	Tool        string    `json:"tool"`
	Server      string    `json:"server"`
	Status      Status    `json:"status"`
	Pinned      string    `json:"pinned_description,omitempty"`
	Current     string    `json:"current_description"`
	PinnedAt    time.Time `json:"pinned_at,omitempty"`
	Fingerprint string    `json:"fingerprint"`
}

// Store holds pins and the status of the tools currently loaded. Safe for
// concurrent use. With a nil database, pins live in memory only and trust on
// first use happens again at every start, which protects nothing across
// restarts; the caller is expected to warn.
type Store struct {
	mu      sync.RWMutex
	db      *sql.DB
	pins    map[string]entry // tool -> accepted version
	servers map[string]bool  // servers with at least one pin
	current map[string]View  // tool -> what is loaded now
}

func NewStore(db *sql.DB) (*Store, error) {
	s := &Store{db: db, pins: map[string]entry{}, servers: map[string]bool{}, current: map[string]View{}}
	if db == nil {
		return s, nil
	}
	rows, err := db.Query(`SELECT tool, server, fingerprint, description, pinned_at FROM tool_pins`)
	if err != nil {
		return nil, fmt.Errorf("load pins: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tool, server, fp, desc, at string
		if err := rows.Scan(&tool, &server, &fp, &desc, &at); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, at)
		s.pins[tool] = entry{Fingerprint: fp, Description: desc, PinnedAt: t}
		s.servers[server] = true
	}
	return s, rows.Err()
}

// Fingerprint hashes what a tool declares about itself: description, the
// raw schema of each parameter, and annotations. Parameters are sorted by
// name so that map order in the upstream's JSON cannot change the hash.
func Fingerprint(t *registry.Tool) string {
	params := make([]registry.Param, len(t.Params))
	copy(params, t.Params)
	sort.Slice(params, func(i, j int) bool { return params[i].Name < params[j].Name })
	type p struct {
		Name     string          `json:"name"`
		Type     string          `json:"type"`
		Required bool            `json:"required"`
		Schema   json.RawMessage `json:"schema,omitempty"`
	}
	ps := make([]p, len(params))
	for i, x := range params {
		ps[i] = p{x.Name, x.Type, x.Required, x.RawSchema}
	}
	b, _ := json.Marshal(struct {
		Description string                `json:"description"`
		Params      []p                   `json:"params"`
		Annotations *registry.Annotations `json:"annotations,omitempty"`
	}{t.Description, ps, t.Annotations})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Observe records the catalogue a server just declared. A server never seen
// before is pinned whole; otherwise each tool is compared to its pin.
func (s *Store) Observe(server string, tools []*registry.Tool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	firstSight := !s.servers[server]
	for _, t := range tools {
		fp := Fingerprint(t)
		v := View{Tool: t.Name, Server: server, Current: t.Description, Fingerprint: fp, Status: Pinned}
		if firstSight {
			if err := s.pinLocked(server, t.Name, fp, t.Description); err != nil {
				return err
			}
		}
		switch pinned, ok := s.pins[t.Name]; {
		case !ok:
			v.Status = New
		case pinned.Fingerprint != fp:
			v.Status = Changed
			v.Pinned, v.PinnedAt = pinned.Description, pinned.PinnedAt
		default:
			v.PinnedAt = pinned.PinnedAt
		}
		s.current[t.Name] = v
	}
	return nil
}

// Floor returns the floor the tool's pin status imposes, "" when none.
func (s *Store) Floor(tool string) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current[tool].Status.Floor()
}

// Status reports the status of a loaded tool, "" when pinning does not
// cover it (CLI and OpenAPI tools, or a tool not loaded).
func (s *Store) Status(tool string) Status {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current[tool].Status
}

// Pending lists loaded tools that are new or changed, sorted by name.
func (s *Store) Pending() []View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []View{}
	for _, v := range s.current {
		if v.Status != Pinned {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}

// Accept pins the currently loaded version of each tool. It returns the
// views as they were before, for the record, and an error naming the first
// tool that is not loaded.
func (s *Store) Accept(tools []string) ([]View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := make([]View, 0, len(tools))
	for _, name := range tools {
		v, ok := s.current[name]
		if !ok {
			return before, fmt.Errorf("tool %q is not loaded from an upstream MCP server", name)
		}
		if err := s.pinLocked(v.Server, name, v.Fingerprint, v.Current); err != nil {
			return before, err
		}
		before = append(before, v)
		v.Status, v.Pinned, v.PinnedAt = Pinned, "", s.pins[name].PinnedAt
		s.current[name] = v
	}
	return before, nil
}

func (s *Store) pinLocked(server, tool, fp, desc string) error {
	now := time.Now().UTC()
	if s.db != nil {
		_, err := s.db.Exec(`INSERT INTO tool_pins (tool, server, fingerprint, description, pinned_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(tool) DO UPDATE SET server=excluded.server, fingerprint=excluded.fingerprint,
				description=excluded.description, pinned_at=excluded.pinned_at`,
			tool, server, fp, desc, now.Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("pin %s: %w", tool, err)
		}
	}
	s.pins[tool] = entry{Fingerprint: fp, Description: desc, PinnedAt: now}
	s.servers[server] = true
	return nil
}
