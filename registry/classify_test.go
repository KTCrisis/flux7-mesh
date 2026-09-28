package registry

import "testing"

func boolp(b bool) *bool { return &b }

func TestClassify(t *testing.T) {
	str := func(name string) Param { return Param{Name: name, Type: "string", Required: true} }

	tests := []struct {
		name   string
		tool   *Tool
		family Family
		access Access
		action string
	}{
		// Named, read.
		{"openapi GET", &Tool{Name: "get_order", Method: "GET"}, FamilyNamed, AccessRead, "allow"},
		{"mcp list", &Tool{Name: "gmail.gmail_list_emails", MCPServer: "gmail"}, FamilyNamed, AccessRead, "allow"},
		{"fs tree", &Tool{Name: "filesystem.directory_tree", MCPServer: "filesystem"}, FamilyNamed, AccessRead, "allow"},
		{"search with a query is not generic",
			&Tool{Name: "searxng.searxng_web_search", MCPServer: "searxng", Params: []Param{str("query")}},
			FamilyNamed, AccessRead, "allow"},
		{"readOnlyHint alone", &Tool{Name: "x.frobnicate", MCPServer: "x",
			Annotations: &Annotations{ReadOnly: boolp(true)}}, FamilyNamed, AccessRead, "allow"},

		// Named, write.
		{"openapi POST", &Tool{Name: "create_order", Method: "POST"}, FamilyNamed, AccessWrite, "human_approval"},
		{"mcp send", &Tool{Name: "gmail.gmail_send_email", MCPServer: "gmail"}, FamilyNamed, AccessWrite, "human_approval"},
		{"write verb beats read verb", &Tool{Name: "gmail.gmail_mark_as_read", MCPServer: "gmail"}, FamilyNamed, AccessWrite, "human_approval"},
		{"destructiveHint beats a read name", &Tool{Name: "x.get_and_clear", MCPServer: "x",
			Annotations: &Annotations{Destructive: boolp(true)}}, FamilyNamed, AccessWrite, "human_approval"},
		{"readOnlyHint=false is a statement", &Tool{Name: "x.list_things", MCPServer: "x",
			Annotations: &Annotations{ReadOnly: boolp(false)}}, FamilyNamed, AccessWrite, "human_approval"},

		// Unknown.
		{"no signal", &Tool{Name: "ollama.generate", MCPServer: "ollama"}, FamilyNamed, AccessUnknown, "human_approval"},
		{"verbs match whole tokens only", &Tool{Name: "x.thread_unset", MCPServer: "x"}, FamilyNamed, AccessUnknown, "human_approval"},

		// Generic.
		{"sql tool", &Tool{Name: "supabase.execute_sql", MCPServer: "supabase", Params: []Param{str("query")}},
			FamilyGeneric, AccessWrite, "human_approval"},
		{"code mode", &Tool{Name: "cloudflare.execute", MCPServer: "cloudflare", Params: []Param{str("code")}},
			FamilyGeneric, AccessWrite, "human_approval"},
		{"shell", &Tool{Name: "sh.shell", MCPServer: "sh", Params: []Param{str("command")}},
			FamilyGeneric, AccessUnknown, "human_approval"},
		{"generic even when it claims read-only", &Tool{Name: "db.query", MCPServer: "db",
			Params: []Param{str("sql")}, Annotations: &Annotations{ReadOnly: boolp(true)}},
			FamilyGeneric, AccessRead, "human_approval"},
		{"cli dispatcher", &Tool{Name: "terraform.__dispatch", Source: "cli",
			CLIMeta: &CLIToolMeta{Bin: "terraform", IsCatchAll: true}}, FamilyGeneric, AccessUnknown, "human_approval"},
		{"non-string code param is not a language", &Tool{Name: "x.lookup", MCPServer: "x",
			Params: []Param{{Name: "code", Type: "integer"}}}, FamilyNamed, AccessRead, "allow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Classify(tt.tool)
			if c.Family != tt.family || c.Access != tt.access {
				t.Errorf("got family=%s access=%s, want family=%s access=%s (reasons %v)",
					c.Family, c.Access, tt.family, tt.access, c.Reasons)
			}
			if got := c.SuggestedAction(); got != tt.action {
				t.Errorf("SuggestedAction() = %s, want %s", got, tt.action)
			}
			if len(c.Reasons) == 0 {
				t.Error("a classification must always say why")
			}
		})
	}
}

func TestClassifyNil(t *testing.T) {
	if c := Classify(nil); c.SuggestedAction() != "human_approval" {
		t.Errorf("nil tool must not be allowed, got %s", c.SuggestedAction())
	}
}
