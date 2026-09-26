package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/KTCrisis/flux7-mesh/trace"
)

// runTrace handles `mesh7 trace <subcommand>`.
func runTrace(args []string) {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(os.Stderr, "usage: mesh7 trace verify [--json] <file>... (oldest first, e.g. traces.jsonl.old traces.jsonl)")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("trace verify", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Print the report as JSON")
	keyEnv := fs.String("key-env", "MESH_TRACE_KEY", "Environment variable holding the HMAC key")
	fs.Parse(args[1:])
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "mesh7 trace verify: no file given")
		os.Exit(2)
	}

	var key []byte
	if v := os.Getenv(*keyEnv); v != "" {
		key = []byte(v)
	}
	r, err := trace.Verify(fs.Args(), key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mesh7 trace verify:", err)
		os.Exit(2)
	}

	if *asJSON {
		out, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Printf("lines %d · chained %d · unchained (before the chain) %d\n", r.Lines, r.Chained, r.Unchained)
		if r.Chained > 0 {
			fmt.Printf("seq %d → %d · alg %s\nhead %s\n", r.FirstSeq, r.LastSeq, r.Alg, r.Head)
			if r.Anchor != "" {
				fmt.Printf("anchor %s (last hash of the previous file)\n", r.Anchor)
			}
		}
		if r.Break != nil {
			fmt.Printf("BROKEN %s:%d seq %d: %s\n", r.Break.File, r.Break.Line, r.Break.Seq, r.Break.Reason)
		} else if r.Chained > 0 {
			fmt.Println("OK")
		}
	}
	if r.Break != nil {
		os.Exit(1)
	}
}
