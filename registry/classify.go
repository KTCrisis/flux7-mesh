package registry

import (
	"fmt"
	"strings"
)

// Family says where the meaning of a call lives.
//
//   - FamilyNamed: the tool name states the effect; arguments refine it.
//   - FamilyGeneric: the tool is an interpreter (SQL, shell, code, a CLI
//     dispatcher); the effect is whatever the argument says, so a policy on
//     the name alone decides nothing useful.
//
// Composed tools (one name, several calls behind an intent) cannot be told
// apart from named ones by their metadata, so they classify as named.
// See docs/internal/design-tool-families.md.
type Family string

const (
	FamilyNamed   Family = "named"
	FamilyGeneric Family = "generic"
)

// Access is the guessed effect of calling the tool.
type Access string

const (
	AccessRead    Access = "read"
	AccessWrite   Access = "write"
	AccessUnknown Access = "unknown"
)

// Classification is a suggestion for a policy author, not a policy. Every
// field comes from what the upstream declares (name, schema, annotations,
// HTTP method), none of which is verified. Reasons list each signal used, so
// a reviewer can check the guess instead of trusting it.
type Classification struct {
	Family  Family   `json:"family"`
	Access  Access   `json:"access"`
	Reasons []string `json:"reasons"`
}

// SuggestedAction is the action a draft policy should start from: allow only
// what reads through a named tool, ask for everything else. Starting strict
// and relaxing on evidence is cheaper than the reverse.
func (c Classification) SuggestedAction() string {
	if c.Family == FamilyNamed && c.Access == AccessRead {
		return "allow"
	}
	return "human_approval"
}

// Verbs are matched on whole tokens of the upstream's own tool name, so
// "thread" does not read as "read" and "unset" does not read as "set".
var readVerbs = setOf(
	"get", "list", "find", "search", "read", "describe", "show", "fetch",
	"query", "view", "lookup", "inspect", "info", "tree", "status", "whoami",
	"count", "stat", "logs", "recall",
)

var writeVerbs = setOf(
	"create", "update", "delete", "remove", "send", "write", "edit", "move",
	"set", "put", "post", "patch", "execute", "exec", "run", "deploy", "drop",
	"insert", "upload", "modify", "rename", "archive", "trash", "apply",
	"destroy", "kill", "stop", "start", "restart", "push", "publish", "forget",
	"store", "save", "mark", "recover", "cast", "approve", "deny", "revoke",
	"purge", "prune", "draft",
)

// Argument names that carry a language rather than a value. "query" is left
// out on purpose: most tools taking a query run a search, not a statement.
var languageParams = setOf(
	"code", "script", "sql", "command", "cmd", "statement", "program",
	"expression",
)

// Verbs that turn a "query" argument into a statement to run.
var runVerbs = setOf("execute", "exec", "run", "sql")

// Classify guesses a tool's family and access from its declared metadata.
//
// Conflicting signals resolve to the more restrictive reading: one write
// signal outweighs any number of read signals, because a false "read" opens a
// hole while a false "write" only costs an approval.
func Classify(t *Tool) Classification {
	c := Classification{Family: FamilyNamed, Access: AccessUnknown}
	if t == nil {
		return c
	}
	tokens := nameTokens(t)

	// Family.
	if t.CLIMeta != nil && t.CLIMeta.IsCatchAll {
		c.Family = FamilyGeneric
		c.Reasons = append(c.Reasons, "CLI dispatcher: the subcommand is an argument")
	}
	for _, p := range t.Params {
		if p.Type != "" && p.Type != "string" {
			continue
		}
		name := strings.ToLower(p.Name)
		switch {
		case languageParams[name]:
			c.Family = FamilyGeneric
			c.Reasons = append(c.Reasons, fmt.Sprintf("argument %q carries a language", p.Name))
		case name == "query" && hasAny(tokens, runVerbs):
			c.Family = FamilyGeneric
			c.Reasons = append(c.Reasons, fmt.Sprintf("argument %q is run, not searched", p.Name))
		}
	}

	// Access signals.
	var read, write bool
	switch strings.ToUpper(t.Method) {
	case "":
	case "GET", "HEAD", "OPTIONS":
		read = true
		c.Reasons = append(c.Reasons, "HTTP "+strings.ToUpper(t.Method))
	default:
		write = true
		c.Reasons = append(c.Reasons, "HTTP "+strings.ToUpper(t.Method))
	}
	if a := t.Annotations; a != nil {
		if a.ReadOnly != nil {
			if *a.ReadOnly {
				read = true
				c.Reasons = append(c.Reasons, "annotation readOnlyHint=true")
			} else {
				write = true
				c.Reasons = append(c.Reasons, "annotation readOnlyHint=false")
			}
		}
		if a.Destructive != nil && *a.Destructive {
			write = true
			c.Reasons = append(c.Reasons, "annotation destructiveHint=true")
		}
		if a.OpenWorld != nil && *a.OpenWorld {
			c.Reasons = append(c.Reasons, "annotation openWorldHint=true: reaches outside systems")
		}
	}
	for _, tok := range tokens {
		if writeVerbs[tok] {
			write = true
			c.Reasons = append(c.Reasons, fmt.Sprintf("name verb %q", tok))
		} else if readVerbs[tok] {
			read = true
			c.Reasons = append(c.Reasons, fmt.Sprintf("name verb %q", tok))
		}
	}

	switch {
	case write:
		c.Access = AccessWrite
	case read:
		c.Access = AccessRead
	default:
		c.Reasons = append(c.Reasons, "no signal")
	}
	return c
}

// nameTokens splits the tool's own name (without the mesh namespace) into
// lowercase words: "gmail.gmail_send_email" gives [gmail send email].
func nameTokens(t *Tool) []string {
	name := t.Name
	if t.MCPServer != "" {
		name = strings.TrimPrefix(name, t.MCPServer+".")
	} else if i := strings.Index(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ' ' || r == '/'
	})
}

func hasAny(tokens []string, set map[string]bool) bool {
	for _, t := range tokens {
		if set[t] {
			return true
		}
	}
	return false
}

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}
