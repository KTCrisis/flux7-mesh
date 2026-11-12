package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
)

// declFingerprint identifies what a declaration would run or call: transport,
// command, arguments, URL, environment (names and values, a value can change
// what a program does) and the names of the headers. Header values are left
// out: they carry tokens that rotate, and a header cannot make the mesh run
// anything.
func declFingerprint(sc config.MCPServerConfig) string {
	envKeys := make([]string, 0, len(sc.Env))
	for k := range sc.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	env := make([][2]string, 0, len(envKeys))
	for _, k := range envKeys {
		env = append(env, [2]string{k, sc.Env[k]})
	}
	headers := make([]string, 0, len(sc.Headers))
	for k := range sc.Headers {
		headers = append(headers, k)
	}
	sort.Strings(headers)
	raw, _ := json.Marshal(struct {
		Name, Transport, Command string
		Args                     []string
		URL                      string
		Env                      [][2]string
		Headers                  []string
		ForwardIdentity          bool
	}{sc.Name, sc.Transport, sc.Command, sc.Args, sc.URL, env, headers, sc.ForwardIdentity})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// acceptedStore remembers, across restarts, which declaration an operator
// accepted for each upstream name. Without a database it remembers nothing
// and every start is a first use.
type acceptedStore struct {
	db *sql.DB

	mu          sync.Mutex
	byName      map[string]string // name → fingerprint
	initialized bool              // a first start has been recorded
}

// initMarker is the row that says the first start happened. No upstream can
// have an empty name.
const initMarker = ""

func newAcceptedStore(db *sql.DB) (*acceptedStore, error) {
	s := &acceptedStore{db: db, byName: map[string]string{}}
	if db == nil {
		return s, nil
	}
	rows, err := db.Query(`SELECT name, fingerprint FROM upstream_accepted`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, fp string
		if err := rows.Scan(&name, &fp); err != nil {
			return nil, err
		}
		if name == initMarker {
			s.initialized = true
			continue
		}
		s.byName[name] = fp
	}
	return s, rows.Err()
}

// persistent says whether acceptances survive a restart.
func (s *acceptedStore) persistent() bool { return s.db != nil }

func (s *acceptedStore) firstUse() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.initialized
}

func (s *acceptedStore) accepted(sc config.MCPServerConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byName[sc.Name] == declFingerprint(sc)
}

func (s *acceptedStore) write(name, fp, by string) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO upstream_accepted (name, fingerprint, accepted_at, accepted_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET fingerprint = excluded.fingerprint, accepted_at = excluded.accepted_at, accepted_by = excluded.accepted_by`,
		name, fp, time.Now().UTC().Format(time.RFC3339), by)
	return err
}

// accept records sc as the accepted declaration of its name.
func (s *acceptedStore) accept(sc config.MCPServerConfig, by string) error {
	fp := declFingerprint(sc)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName[sc.Name] = fp
	return s.write(sc.Name, fp, by)
}

// markInitialized records that the first start happened.
func (s *acceptedStore) markInitialized() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialized = true
	return s.write(initMarker, "-", "first-start")
}

// forget drops the acceptance of a name: declared again later, it is new.
func (s *acceptedStore) forget(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byName, name)
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM upstream_accepted WHERE name = ?`, name)
	return err
}

// names lists the upstream names that have an accepted declaration.
func (s *acceptedStore) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.byName))
	for n := range s.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
