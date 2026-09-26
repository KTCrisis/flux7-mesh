package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

var meshURL = "http://localhost:9090"

func init() {
	if u := os.Getenv("MESH_URL"); u != "" {
		meshURL = u
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "pending":
		cmdPending()
	case "show":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: mesh show <id>")
			os.Exit(1)
		}
		cmdShow(os.Args[2])
	case "approve":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: mesh approve <id> [--grant <duration>] [--tools <glob>]")
			os.Exit(1)
		}
		cmdApprove(os.Args[2], os.Args[3:])
	case "deny":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: mesh deny <id>")
			os.Exit(1)
		}
		resolve(os.Args[2], "deny", false)
	case "watch":
		cmdWatch()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: mesh <command> [args]

commands:
  pending              List pending approvals
  show <id>            Show approval details
  approve <id>         Approve a pending request
  deny <id>            Deny a pending request
  watch                Interactive mode — poll and prompt for each approval

approve flags:
  --grant <duration>   Also open a temporal grant, so the same call does not
                       ask again (e.g. 30m, 1h). The grant records this
                       approval as its origin, which is what lets
                       GET /traces/{id}/why (and the console) walk back
                       to this decision later.
  --tools <glob>       Tool pattern for that grant (default: the exact tool
                       approved). Widening it is deliberate.

env:
  MESH_URL             Agent-mesh URL (default http://localhost:9090)
  MESH_GRANT_DURATION  Duration used by [g] in watch (default 1h)`)
}

type approvalView struct {
	ID         string         `json:"id"`
	AgentID    string         `json:"agent_id"`
	Tool       string         `json:"tool"`
	Params     map[string]any `json:"params"`
	TraceID    string         `json:"trace_id,omitempty"`
	PolicyRule string         `json:"policy_rule"`
	Status     string         `json:"status"`
	CreatedAt  time.Time      `json:"created_at"`
	Remaining  string         `json:"remaining,omitempty"`
	ResolvedBy string         `json:"resolved_by,omitempty"`
}

func cmdPending() {
	resp, err := http.Get(meshURL + "/approvals?status=pending")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var list []approvalView
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		fmt.Fprintf(os.Stderr, "error decoding response: %v\n", err)
		os.Exit(1)
	}

	if len(list) == 0 {
		fmt.Println("no pending approvals")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tAGE\tAGENT\tTOOL\tREMAINING")
	for _, a := range list {
		age := time.Since(a.CreatedAt).Truncate(time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			a.ID[:8], age, a.AgentID, a.Tool, a.Remaining)
	}
	w.Flush()
}

func cmdShow(id string) {
	resp, err := http.Get(meshURL + "/approvals/" + id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		fmt.Fprintf(os.Stderr, "approval %s not found\n", id)
		os.Exit(1)
	}

	var a approvalView
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		fmt.Fprintf(os.Stderr, "error decoding response: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("ID:       %s\n", a.ID)
	fmt.Printf("Agent:    %s\n", a.AgentID)
	fmt.Printf("Tool:     %s\n", a.Tool)
	fmt.Printf("Policy:   %s\n", a.PolicyRule)
	fmt.Printf("Status:   %s\n", a.Status)
	fmt.Printf("Age:      %s\n", time.Since(a.CreatedAt).Truncate(time.Second))
	if a.Remaining != "" {
		fmt.Printf("Timeout:  %s remaining\n", a.Remaining)
	}
	if len(a.Params) > 0 {
		fmt.Println("Params:")
		for k, v := range a.Params {
			s := fmt.Sprintf("%v", v)
			if len(s) > 120 {
				s = s[:120] + "..."
			}
			fmt.Printf("  %s: %s\n", k, s)
		}
	}
}

// cmdApprove approves a request and, with --grant, extends the decision into a
// temporal grant carrying this approval as its origin.
//
// The approval is fetched BEFORE resolving it: that is the moment its agent,
// tool and trace ID are certainly readable, and it also turns a prefix into the
// full ID a grant must record.
func cmdApprove(id string, flags []string) {
	duration, tools, err := parseApproveFlags(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var target *approvalView
	if duration != "" {
		target = fetchApproval(id)
		if target == nil {
			fmt.Fprintf(os.Stderr, "approval %s not found\n", id)
			os.Exit(1)
		}
	}

	resolve(id, "approve", false)

	if duration == "" {
		return
	}
	if tools == "" {
		// The exact tool, not a glob. An operator who wants to widen the grant
		// says so with --tools; nobody widens it by accident.
		tools = target.Tool
	}
	createGrant(target, tools, duration, false)
}

func parseApproveFlags(flags []string) (duration, tools string, err error) {
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case "--grant":
			if i+1 >= len(flags) {
				return "", "", fmt.Errorf("--grant needs a duration (e.g. 1h)")
			}
			duration = flags[i+1]
			i++
		case "--tools":
			if i+1 >= len(flags) {
				return "", "", fmt.Errorf("--tools needs a pattern")
			}
			tools = flags[i+1]
			i++
		default:
			return "", "", fmt.Errorf("unknown flag: %s", flags[i])
		}
	}
	if tools != "" && duration == "" {
		return "", "", fmt.Errorf("--tools only makes sense with --grant")
	}
	if duration != "" {
		if _, e := time.ParseDuration(duration); e != nil {
			return "", "", fmt.Errorf("invalid duration %q: %v", duration, e)
		}
	}
	return duration, tools, nil
}

func fetchApproval(id string) *approvalView {
	resp, err := http.Get(meshURL + "/approvals/" + id)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var a approvalView
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return nil
	}
	return &a
}

// createGrant opens a temporal grant that names the approval it came from.
// A failure here never fails the approval: the call was already let through,
// and losing the shortcut is a lesser harm than pretending the decision failed.
func createGrant(a *approvalView, tools, duration string, interactive bool) {
	body, _ := json.Marshal(map[string]string{
		"agent":       a.AgentID,
		"tools":       tools,
		"duration":    duration,
		"approval_id": a.ID,
		"trace_id":    a.TraceID,
	})

	indent := ""
	if interactive {
		indent = "  "
	}

	resp, err := http.Post(meshURL+"/grants", "application/json", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%sapproved, but the grant failed: %v\n", indent, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		fmt.Fprintf(os.Stderr, "%sapproved, but the grant failed: status %d\n", indent, resp.StatusCode)
		return
	}

	var g struct {
		ID        string `json:"id"`
		Remaining string `json:"remaining"`
	}
	json.NewDecoder(resp.Body).Decode(&g)

	fmt.Printf("%sGrant %s — %s on %s for %s\n", indent, short(g.ID), a.AgentID, tools, g.Remaining)
	if a.TraceID == "" {
		// Say it out loud rather than let the operator believe the chain is
		// complete. An approval with no trace predates the wiring, or came
		// through a path that does not carry one.
		fmt.Printf("%s  (no origin trace on this approval — the chain will stop here)\n", indent)
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func grantDuration() string {
	if d := os.Getenv("MESH_GRANT_DURATION"); d != "" {
		return d
	}
	return "1h"
}

func cmdWatch() {
	fmt.Println("mesh watch — waiting for approvals (ctrl+c to quit)")
	fmt.Println()

	seen := make(map[string]bool)
	scanner := bufio.NewScanner(os.Stdin)

	for {
		list := fetchPending()

		for _, a := range list {
			if seen[a.ID] {
				continue
			}

			// Show the new approval
			fmt.Printf("\033[1;33m>> NEW\033[0m  %s  %s  %s\n", a.ID[:8], a.AgentID, a.Tool)
			if len(a.Params) > 0 {
				for k, v := range a.Params {
					s := fmt.Sprintf("%v", v)
					if len(s) > 80 {
						s = s[:80] + "..."
					}
					fmt.Printf("        %s: %s\n", k, s)
				}
			}
			fmt.Printf("        remaining: %s\n", a.Remaining)

			// Prompt
			for {
				fmt.Printf("  [a]pprove / [g]rant %s / [d]eny / [s]kip ? ", grantDuration())
				if !scanner.Scan() {
					return
				}
				input := strings.TrimSpace(strings.ToLower(scanner.Text()))

				switch input {
				case "a", "approve":
					resolve(a.ID[:8], "approve", true)
					seen[a.ID] = true
				case "g", "grant":
					// Approve, then stop being asked about this exact tool for
					// a while. The grant records this approval as its origin,
					// so the calls it later waves through stay traceable to
					// the decision taken here.
					resolve(a.ID[:8], "approve", true)
					approved := a
					createGrant(&approved, a.Tool, grantDuration(), true)
					seen[a.ID] = true
				case "d", "deny":
					resolve(a.ID[:8], "deny", true)
					seen[a.ID] = true
				case "s", "skip":
					seen[a.ID] = true
				default:
					fmt.Println("  type a, g, d, or s")
					continue
				}
				break
			}
			fmt.Println()
		}

		time.Sleep(2 * time.Second)
	}
}

func fetchPending() []approvalView {
	resp, err := http.Get(meshURL + "/approvals?status=pending")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var list []approvalView
	_ = json.NewDecoder(resp.Body).Decode(&list) // best-effort in watch loop
	return list
}

// resolve posts an approve/deny action. In interactive mode (watch), errors are
// printed inline. In CLI mode, errors cause exit(1).
func resolve(id string, action string, interactive bool) {
	body := fmt.Sprintf(`{"resolved_by":"cli:%s"}`, os.Getenv("USER"))
	resp, err := http.Post(meshURL+"/approvals/"+id+"/"+action,
		"application/json", strings.NewReader(body))
	if err != nil {
		if interactive {
			fmt.Fprintf(os.Stderr, "  error: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	defer resp.Body.Close()

	verb := "Approved"
	if action == "deny" {
		verb = "Denied"
	}

	switch resp.StatusCode {
	case 200:
		if interactive {
			fmt.Printf("  %s\n", verb)
		} else {
			fmt.Printf("%s: %s\n", verb, id)
		}
	case 404:
		if interactive {
			fmt.Fprintf(os.Stderr, "  not found\n")
		} else {
			fmt.Fprintf(os.Stderr, "approval %s not found\n", id)
			os.Exit(1)
		}
	case 409:
		if interactive {
			fmt.Println("  already resolved")
		} else {
			fmt.Fprintf(os.Stderr, "approval %s already resolved\n", id)
			os.Exit(1)
		}
	default:
		if interactive {
			fmt.Fprintf(os.Stderr, "  error: status %d\n", resp.StatusCode)
		} else {
			fmt.Fprintf(os.Stderr, "unexpected status: %d\n", resp.StatusCode)
			os.Exit(1)
		}
	}
}
