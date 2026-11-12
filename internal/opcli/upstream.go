package opcli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"
)

type upstreamView struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Status    string   `json:"status"`
	Error     string   `json:"error,omitempty"`
	Tools     []string `json:"tools"`
}

type upstreamDiff struct {
	Added     []string `json:"added"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
	Unchanged []string `json:"unchanged"`
	Held      []string `json:"held"`
}

type heldView struct {
	Name       string   `json:"name"`
	Transport  string   `json:"transport"`
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	URL        string   `json:"url"`
	EnvKeys    []string `json:"env_keys"`
	HeaderKeys []string `json:"header_keys"`
	Replaces   bool     `json:"replaces"`
}

// printHeld shows each held declaration as the operator must read it before
// accepting: the program that would run, or the address that would be called.
func printHeld(w io.Writer, held []heldView) {
	for _, h := range held {
		what := "new server"
		if h.Replaces {
			what = "replaces the running declaration"
		}
		fmt.Fprintf(w, "%s  (%s, %s)\n", h.Name, h.Transport, what)
		if h.Command != "" {
			fmt.Fprintf(w, "    runs     %s\n", strings.Join(append([]string{h.Command}, h.Args...), " "))
		}
		if h.URL != "" {
			fmt.Fprintf(w, "    calls    %s\n", h.URL)
		}
		if len(h.EnvKeys) > 0 {
			fmt.Fprintf(w, "    env      %s\n", strings.Join(h.EnvKeys, ", "))
		}
		if len(h.HeaderKeys) > 0 {
			fmt.Fprintf(w, "    headers  %s\n", strings.Join(h.HeaderKeys, ", "))
		}
	}
}

// call runs one control-plane request and decodes its JSON answer into out
// (nil to ignore it). An answer that is not 2xx becomes an error carrying the
// mesh's own message.
func call(method, path, body string, out any) error {
	resp, err := adminDo(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// cmdUpstream serves `upstream list | reload | restart <name>` and returns the
// exit code.
func cmdUpstream(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintf(stderr, "usage: %s upstream list | reload | held | accept <name> | restart <name>\n", prog)
		return 1
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "list":
		var servers []upstreamView
		if err := call("GET", "/mcp-servers", "", &servers); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tTRANSPORT\tSTATUS\tTOOLS\tERROR")
		for _, s := range servers {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", s.Name, s.Transport, s.Status, len(s.Tools), s.Error)
		}
		tw.Flush()
		return 0

	case "reload":
		var res struct {
			Diff upstreamDiff `json:"diff"`
		}
		if err := call("POST", "/mcp-servers/reload", "", &res); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		d := res.Diff
		if len(d.Added)+len(d.Changed)+len(d.Removed)+len(d.Held) == 0 {
			fmt.Fprintf(stdout, "nothing changed (%d upstream(s) left as they were)\n", len(d.Unchanged))
			return 0
		}
		for _, line := range []struct {
			label string
			names []string
		}{{"added", d.Added}, {"changed", d.Changed}, {"removed", d.Removed}} {
			if len(line.names) > 0 {
				fmt.Fprintf(stdout, "%-8s %s\n", line.label, strings.Join(line.names, ", "))
			}
		}
		fmt.Fprintf(stdout, "%-8s %d\n", "untouched", len(d.Unchanged))
		if len(d.Held) > 0 {
			fmt.Fprintf(stdout, "\nheld, nothing started: %s\n", strings.Join(d.Held, ", "))
			var held []heldView
			if call("GET", "/mcp-servers/held", "", &held) == nil {
				printHeld(stdout, held)
			}
			fmt.Fprintf(stdout, "\nread what each would run, then: %s upstream accept <name>\n", prog)
		}
		return 0

	case "held":
		var held []heldView
		if err := call("GET", "/mcp-servers/held", "", &held); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if len(held) == 0 {
			fmt.Fprintln(stdout, "no declaration held")
			return 0
		}
		printHeld(stdout, held)
		return 0

	case "accept":
		if len(args) != 2 {
			return usage()
		}
		if err := call("POST", "/mcp-servers/"+url.PathEscape(args[1])+"/accept", "", nil); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s accepted and started\n", args[1])
		return 0

	case "restart":
		if len(args) != 2 {
			return usage()
		}
		if err := call("POST", "/mcp-servers/"+url.PathEscape(args[1])+"/restart", "", nil); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s restarted\n", args[1])
		return 0
	}
	return usage()
}

type pinView struct {
	Tool   string `json:"tool"`
	Server string `json:"server"`
	Status string `json:"status"`
}

// cmdPin serves `pin list | accept <tool>... | accept --server <name>` and
// returns the exit code.
func cmdPin(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintf(stderr, "usage: %s pin list | accept <tool>... | accept --server <name>\n", prog)
		return 1
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "list":
		var pending []pinView
		if err := call("GET", "/tools/pins", "", &pending); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if len(pending) == 0 {
			fmt.Fprintln(stdout, "no tool held back")
			return 0
		}
		sort.Slice(pending, func(i, j int) bool { return pending[i].Tool < pending[j].Tool })
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TOOL\tSERVER\tSTATUS")
		for _, p := range pending {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Tool, p.Server, p.Status)
		}
		tw.Flush()
		return 0

	case "accept":
		req := map[string]any{"by": prog + " cli"}
		rest := args[1:]
		switch {
		case len(rest) == 2 && rest[0] == "--server":
			req["server"] = rest[1]
		case len(rest) > 0 && !strings.HasPrefix(rest[0], "-"):
			req["tools"] = rest
		default:
			return usage()
		}
		body, _ := json.Marshal(req)
		if err := call("POST", "/tools/pins/accept", string(body), nil); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if s, ok := req["server"]; ok {
			fmt.Fprintf(stdout, "accepted the held tools of %s\n", s)
		} else {
			fmt.Fprintf(stdout, "accepted %s\n", strings.Join(rest, ", "))
		}
		return 0
	}
	return usage()
}
