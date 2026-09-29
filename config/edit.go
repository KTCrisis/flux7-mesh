package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scalar is one value to set in a two-level section of the config file,
// e.g. {Section: "approval", Key: "wait_seconds", Value: "3"}.
type Scalar struct {
	Section string
	Key     string
	Value   string // YAML text, written as is
}

// SetScalars changes values in the config text line by line, located through
// the YAML tree, so comments, blank lines and key order survive (a
// re-serialization would lose them). A key that exists keeps its line and its
// trailing comment; a missing key is added at the end of its section; a
// missing section is appended to the file.
func SetScalars(src []byte, updates []Scalar) ([]byte, error) {
	out := src
	for _, u := range updates {
		var err error
		if out, err = setScalar(out, u); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func setScalar(src []byte, u Scalar) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("config is not valid YAML: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config root is not a mapping")
	}
	root := doc.Content[0]
	lines := strings.Split(string(src), "\n")

	var section *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == u.Section {
			section = root.Content[i+1]
			break
		}
	}
	if section == nil {
		text := strings.TrimRight(string(src), "\n")
		return []byte(fmt.Sprintf("%s\n\n%s:\n  %s: %s\n", text, u.Section, u.Key, u.Value)), nil
	}
	if section.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config section %q is not a mapping", u.Section)
	}

	for i := 0; i+1 < len(section.Content); i += 2 {
		key, val := section.Content[i], section.Content[i+1]
		if key.Value != u.Key {
			continue
		}
		if val.Kind != yaml.ScalarNode || val.Line != key.Line {
			return nil, fmt.Errorf("%s.%s is not a one-line value; edit it in the file", u.Section, u.Key)
		}
		idx := val.Line - 1
		line := lines[idx]
		comment := ""
		if val.LineComment != "" {
			comment = " " + val.LineComment
		}
		lines[idx] = line[:val.Column-1] + u.Value + comment
		return []byte(strings.Join(lines, "\n")), nil
	}

	// missing key: after the section's last line, at its keys' indentation
	indent := 2
	last := section.Line
	if len(section.Content) > 0 {
		indent = section.Content[0].Column - 1
		last = lastLine(section)
	}
	entry := strings.Repeat(" ", indent) + u.Key + ": " + u.Value
	lines = append(lines[:last], append([]string{entry}, lines[last:]...)...)
	return []byte(strings.Join(lines, "\n")), nil
}

// lastLine is the last source line a node spans (1-based).
func lastLine(n *yaml.Node) int {
	l := n.Line
	for _, c := range n.Content {
		if cl := lastLine(c); cl > l {
			l = cl
		}
	}
	return l
}
