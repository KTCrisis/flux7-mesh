package policy

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConsoleMarker heads every rule written through the control plane. Only rules
// carrying it can be removed that way: a rule written by hand stays the
// operator's, whatever the console thinks of it.
const ConsoleMarker = "# set from console"

// Policy files are edited as text, located through the YAML tree. Re-emitting
// the tree would lose blank lines and reflow hand-written files; editing lines
// leaves everything else byte for byte as the operator wrote it.

type ruleSpan struct {
	node  *yaml.Node
	first int // 1-based line of "- ", after any head comment
	last  int // 1-based last line of the rule's content
}

// rules locates the rules sequence of a single-policy file.
func rules(src []byte) ([]ruleSpan, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("policy file is not a mapping")
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "rules" {
			continue
		}
		seq := root.Content[i+1]
		if seq.Kind != yaml.SequenceNode {
			return nil, nil, errors.New("rules is not a list")
		}
		spans := make([]ruleSpan, len(seq.Content))
		for j, item := range seq.Content {
			spans[j] = ruleSpan{node: item, first: item.Line, last: lastLine(item)}
		}
		return spans, root.Content[i], nil
	}
	return nil, nil, errors.New("policy file has no rules")
}

func lastLine(n *yaml.Node) int {
	max := n.Line
	for _, c := range n.Content {
		if l := lastLine(c); l > max {
			max = l
		}
	}
	return max
}

// ExactRule reports the index of the first rule that names only this tool and
// carries no condition, or -1.
func ExactRule(src []byte, tool string) (int, error) {
	spans, _, err := rules(src)
	if err != nil {
		return -1, err
	}
	for i, s := range spans {
		if isExact(s.node, tool) {
			return i, nil
		}
	}
	return -1, nil
}

func isExact(rule *yaml.Node, tool string) bool {
	if rule.Kind != yaml.MappingNode {
		return false
	}
	var tools *yaml.Node
	for i := 0; i+1 < len(rule.Content); i += 2 {
		switch rule.Content[i].Value {
		case "condition":
			return false
		case "tools":
			tools = rule.Content[i+1]
		}
	}
	return tools != nil && tools.Kind == yaml.SequenceNode &&
		len(tools.Content) == 1 && tools.Content[0].Value == tool
}

// InsertRule inserts `tools: [tool], action: action` before rule `at`, or
// after the last rule when at is -1 or past the end. The rule is headed by
// ConsoleMarker and the given note.
func InsertRule(src []byte, tool, action string, at int, note string) ([]byte, error) {
	spans, rulesKey, err := rules(src)
	if err != nil {
		return nil, err
	}
	lines := splitLines(src)

	var insertAt, indent int // insertAt: 0-based index in lines
	switch {
	case len(spans) == 0:
		return nil, fmt.Errorf("line %d: rules is empty, add a first rule by hand", rulesKey.Line)
	case at < 0 || at >= len(spans):
		last := spans[len(spans)-1]
		insertAt, indent = last.last, last.node.Column-3
		// A value can close on a later line than its last scalar, such as a
		// flow list whose "]" stands alone: step over deeper-indented lines.
		for insertAt < len(lines) && strings.TrimSpace(lines[insertAt]) != "" &&
			leading(lines[insertAt]) > indent && !strings.HasPrefix(strings.TrimSpace(lines[insertAt]), "#") {
			insertAt++
		}
	default:
		insertAt, indent = spans[at].first-1, spans[at].node.Column-3
		// Keep a comment that heads the displaced rule attached to it.
		for insertAt > 0 && strings.HasPrefix(strings.TrimSpace(lines[insertAt-1]), "#") {
			insertAt--
		}
	}
	if indent < 0 {
		indent = 0
	}
	pad := strings.Repeat(" ", indent)
	block := []string{
		pad + ConsoleMarker + ", " + note,
		pad + "- tools: [" + strconv.Quote(tool) + "]",
		pad + "  action: " + action,
	}
	out := append([]string{}, lines[:insertAt]...)
	out = append(out, block...)
	out = append(out, lines[insertAt:]...)
	return joinLines(out, src), nil
}

var actionLine = regexp.MustCompile(`^(\s*(?:-\s+)?action:\s*)("[^"]*"|'[^']*'|[^\s#]+)(.*)$`)

// SetAction rewrites the action of rule `at` in place, keeping its comments.
func SetAction(src []byte, at int, action string) ([]byte, error) {
	spans, _, err := rules(src)
	if err != nil {
		return nil, err
	}
	if at < 0 || at >= len(spans) {
		return nil, fmt.Errorf("rule %d out of range", at)
	}
	rule := spans[at].node
	for i := 0; i+1 < len(rule.Content); i += 2 {
		if rule.Content[i].Value != "action" {
			continue
		}
		lines := splitLines(src)
		idx := rule.Content[i+1].Line - 1
		m := actionLine.FindStringSubmatch(lines[idx])
		if m == nil {
			return nil, fmt.Errorf("line %d: action is not on a line of its own", idx+1)
		}
		lines[idx] = m[1] + action + m[3]
		return joinLines(lines, src), nil
	}
	return nil, fmt.Errorf("rule %d has no action", at)
}

// RemoveConsoleRule deletes the first rule for exactly this tool that is
// headed by ConsoleMarker. It reports false, with src unchanged, when there is
// none: a hand-written rule is never removed from here.
func RemoveConsoleRule(src []byte, tool string) ([]byte, bool, error) {
	spans, _, err := rules(src)
	if err != nil {
		return nil, false, err
	}
	lines := splitLines(src)
	for _, s := range spans {
		if !isExact(s.node, tool) {
			continue
		}
		head := s.first - 2 // 0-based index of the line above "- "
		if head < 0 || !strings.HasPrefix(strings.TrimSpace(lines[head]), ConsoleMarker) {
			continue
		}
		out := append([]string{}, lines[:head]...)
		out = append(out, lines[s.last:]...)
		return joinLines(out, src), true, nil
	}
	return src, false, nil
}

func leading(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func splitLines(src []byte) []string {
	s := strings.TrimSuffix(string(src), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinLines(lines []string, orig []byte) []byte {
	out := strings.Join(lines, "\n")
	if len(orig) == 0 || orig[len(orig)-1] == '\n' {
		out += "\n"
	}
	return []byte(out)
}
