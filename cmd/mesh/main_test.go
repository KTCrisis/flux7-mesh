package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseApproveFlags(t *testing.T) {
	tests := []struct {
		name         string
		flags        []string
		wantDuration string
		wantTools    string
		wantErr      bool
	}{
		{"no flags", nil, "", "", false},
		{"grant only", []string{"--grant", "1h"}, "1h", "", false},
		{"grant and tools", []string{"--grant", "30m", "--tools", "fs.*"}, "30m", "fs.*", false},
		{"grant without duration", []string{"--grant"}, "", "", true},
		{"tools without grant", []string{"--tools", "fs.*"}, "", "", true},
		{"invalid duration", []string{"--grant", "banana"}, "", "", true},
		{"unknown flag", []string{"--forever"}, "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, tools, err := parseApproveFlags(tc.flags)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %v", tc.flags)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d != tc.wantDuration || tools != tc.wantTools {
				t.Errorf("got (%q, %q), want (%q, %q)", d, tools, tc.wantDuration, tc.wantTools)
			}
		})
	}
}

// The grant must carry the approval and the call it came from. This is the
// whole point of the wiring: without these two fields the grant is an orphan
// again and "why was this allowed?" has no answer.
func TestCreateGrantSendsOrigin(t *testing.T) {
	var got map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grants" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"id": "grant123456", "remaining": "1h0m0s"})
	}))
	defer srv.Close()

	old := meshURL
	meshURL = srv.URL
	defer func() { meshURL = old }()

	a := &approvalView{
		ID:      "appr-full-id",
		AgentID: "claude",
		Tool:    "filesystem.write_file",
		TraceID: "0123456789abcdef0123456789abcdef",
	}
	createGrant(a, "filesystem.write_file", "1h", false)

	if got["approval_id"] != "appr-full-id" {
		t.Errorf("approval_id = %q, want the full approval id", got["approval_id"])
	}
	if got["trace_id"] != a.TraceID {
		t.Errorf("trace_id = %q, want %q", got["trace_id"], a.TraceID)
	}
	if got["agent"] != "claude" {
		t.Errorf("agent = %q, want claude", got["agent"])
	}
	if got["tools"] != "filesystem.write_file" {
		t.Errorf("tools = %q, want the exact tool by default", got["tools"])
	}
}

// A failing grant must not be reported as a failed approval: the call was
// already let through.
func TestCreateGrantFailureIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	old := meshURL
	meshURL = srv.URL
	defer func() { meshURL = old }()

	// The assertion is that this returns at all — createGrant must not exit.
	createGrant(&approvalView{ID: "a1", AgentID: "claude", Tool: "fs.read"}, "fs.read", "1h", false)
}

func TestFetchApprovalCarriesTraceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/approvals/abc" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(approvalView{
			ID: "abc-full", AgentID: "claude", Tool: "fs.write",
			TraceID: "0123456789abcdef0123456789abcdef",
		})
	}))
	defer srv.Close()

	old := meshURL
	meshURL = srv.URL
	defer func() { meshURL = old }()

	a := fetchApproval("abc")
	if a == nil {
		t.Fatal("expected an approval")
	}
	if a.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("trace id lost in decoding: %q", a.TraceID)
	}
	if a.ID != "abc-full" {
		t.Errorf("expected the full id, got %q", a.ID)
	}
}

func TestFetchApprovalMissingReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	old := meshURL
	meshURL = srv.URL
	defer func() { meshURL = old }()

	if a := fetchApproval("nope"); a != nil {
		t.Errorf("expected nil for a missing approval, got %+v", a)
	}
}

func TestGrantDurationDefaultsToOneHour(t *testing.T) {
	t.Setenv("MESH_GRANT_DURATION", "")
	if d := grantDuration(); d != "1h" {
		t.Errorf("default duration = %q, want 1h", d)
	}
	t.Setenv("MESH_GRANT_DURATION", "15m")
	if d := grantDuration(); d != "15m" {
		t.Errorf("env override ignored, got %q", d)
	}
}

func TestParseHaltFlags(t *testing.T) {
	cases := []struct {
		args    []string
		scope   string
		target  string
		reason  string
		wantErr bool
	}{
		{[]string{"--all"}, "all", "", "", false},
		{[]string{"--agent", "scout7", "--reason", "loops"}, "agent", "scout7", "loops", false},
		{[]string{"--session", "s-42"}, "session", "s-42", "", false},
		{[]string{}, "", "", "", true},                           // no scope
		{[]string{"--all", "--agent", "x"}, "", "", "", true},    // two scopes
		{[]string{"--agent"}, "", "", "", true},                  // no value
		{[]string{"--agent", "--reason", "x"}, "", "", "", true}, // flag taken as value
		{[]string{"--everything"}, "", "", "", true},             // unknown flag
	}
	for _, c := range cases {
		req, err := parseHaltFlags(c.args)
		if (err != nil) != c.wantErr {
			t.Fatalf("%v: err = %v, wantErr %v", c.args, err, c.wantErr)
		}
		if !c.wantErr && (req.Scope != c.scope || req.Target != c.target || req.Reason != c.reason) {
			t.Fatalf("%v: got %+v", c.args, req)
		}
	}
}
