package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// haltView mirrors what GET /halts and POST /halts return.
type haltView struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	Target    string    `json:"target"`
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// haltRequest is the body of POST /halts.
type haltRequest struct {
	Scope  string `json:"scope"`
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`
	By     string `json:"by"`
}

// parseHaltFlags reads --all | --agent <id> | --session <id> and --reason.
// Exactly one scope is required: an emergency stop must say what it stops.
func parseHaltFlags(args []string) (haltRequest, error) {
	var req haltRequest
	scopes := 0
	for i := 0; i < len(args); i++ {
		value := func() (string, error) {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", fmt.Errorf("%s needs a value", args[i])
			}
			i++
			return args[i], nil
		}
		switch args[i] {
		case "--all":
			req.Scope = "all"
			scopes++
		case "--agent", "--session":
			v, err := value()
			if err != nil {
				return req, err
			}
			req.Scope, req.Target = strings.TrimPrefix(args[i-1], "--"), v
			scopes++
		case "--reason":
			v, err := value()
			if err != nil {
				return req, err
			}
			req.Reason = v
		default:
			return req, fmt.Errorf("unknown flag: %s", args[i])
		}
	}
	if scopes != 1 {
		return req, fmt.Errorf("give exactly one of --all, --agent <id>, --session <id>")
	}
	return req, nil
}

func cmdHalt(args []string) {
	req, err := parseHaltFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\nusage: mesh halt --all | --agent <id> | --session <id> [--reason \"...\"]\n", err)
		os.Exit(1)
	}
	req.By = "cli:" + os.Getenv("USER")
	body, _ := json.Marshal(req)
	resp, err := adminDo("POST", "/halts", string(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var out struct {
		Halt            haltView `json:"halt"`
		AlreadyActive   bool     `json:"already_active"`
		RevokedGrants   int      `json:"revoked_grants"`
		DeniedApprovals int      `json:"denied_approvals"`
		Error           string   `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	switch resp.StatusCode {
	case 201:
		fmt.Printf("\033[1;31mHALTED\033[0m %s  (halt %s)\n", describe(out.Halt), short(out.Halt.ID))
		fmt.Printf("  %d grant(s) revoked, %d pending approval(s) denied\n", out.RevokedGrants, out.DeniedApprovals)
		fmt.Printf("  lift it with: mesh resume %s\n", short(out.Halt.ID))
	case 200:
		fmt.Printf("already halted: %s (halt %s)\n", describe(out.Halt), short(out.Halt.ID))
	default:
		fmt.Fprintf(os.Stderr, "halt failed: status %d %s\n", resp.StatusCode, out.Error)
		os.Exit(1)
	}
}

func cmdHalts() {
	resp, err := adminDo("GET", "/halts", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "unexpected status: %d\n", resp.StatusCode)
		os.Exit(1)
	}
	var list []haltView
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		fmt.Fprintf(os.Stderr, "error decoding response: %v\n", err)
		os.Exit(1)
	}
	if len(list) == 0 {
		fmt.Println("no emergency stop in force")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSINCE\tSTOPS\tBY\tREASON")
	for _, h := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", short(h.ID),
			time.Since(h.CreatedAt).Truncate(time.Second), describe(h), h.CreatedBy, h.Reason)
	}
	w.Flush()
}

func cmdResume(id string) {
	body, _ := json.Marshal(map[string]string{"by": "cli:" + os.Getenv("USER")})
	resp, err := adminDo("POST", "/halts/"+id+"/resume", string(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var out struct {
		Halt           haltView `json:"halt"`
		RestoredGrants int      `json:"restored_grants"`
		Error          string   `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	switch resp.StatusCode {
	case 200:
		fmt.Printf("resumed: %s (halt %s), %d grant(s) restored\n", describe(out.Halt), short(out.Halt.ID), out.RestoredGrants)
	case 404:
		fmt.Fprintf(os.Stderr, "no active halt %s\n", id)
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "resume failed: status %d %s\n", resp.StatusCode, out.Error)
		os.Exit(1)
	}
}

func describe(h haltView) string {
	switch h.Scope {
	case "all":
		return "all agents"
	case "agent":
		return "agent " + h.Target
	case "session":
		return "session " + h.Target
	}
	return h.Scope
}

// adminDo calls the control plane, with the admin token when one is set.
func adminDo(method, path, body string) (*http.Response, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, meshURL+path, rd)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := os.Getenv("MESH_ADMIN_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return http.DefaultClient.Do(req)
}
