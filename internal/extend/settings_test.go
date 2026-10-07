package extend

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestSettingsMergeUserThenProject(t *testing.T) {
	dirs := newLayout(t)
	writeFile(t, filepath.Join(dirs.userDir, settingsFileName), `{
		"permissions": {"allow": ["bash(go test*)"], "deny": ["bash(rm *)"]},
		"hooks": {
			"before_tool": [{"command": "user-before", "tools": ["bash"]}],
			"prompt": [{"command": "user-prompt"}]
		},
		"mcp_servers": {
			"shared": {"command": "user-shared", "args": ["-v"]},
			"zeta": {"command": "zeta-server", "description": "last"}
		}
	}`)
	writeFile(t, filepath.Join(dirs.projectDir(), settingsFileName), `{
		"permissions": {"allow": ["read_file"]},
		"hooks": {
			"before_tool": [{"command": "project-before"}],
			"after_tool": [{"command": "project-after", "tools": ["edit_file", "write_file"]}]
		},
		"mcp_servers": {
			"shared": {"command": "project-shared", "env": {"TOKEN": "x"}},
			"alpha": {"command": "alpha-server"}
		}
	}`)

	settings, err := loadSettings(dirs.userDir, dirs.workDir)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		Allow: []string{"bash(go test*)", "read_file"},
		Deny:  []string{"bash(rm *)"},
		Hooks: Hooks{
			BeforeTool: []Hook{{Command: "user-before", Tools: []string{"bash"}}, {Command: "project-before"}},
			AfterTool:  []Hook{{Command: "project-after", Tools: []string{"edit_file", "write_file"}}},
			Prompt:     []Hook{{Command: "user-prompt"}},
		},
		MCPServers: []MCPServer{
			{Name: "alpha", Command: "alpha-server"},
			{Name: "shared", Command: "project-shared", Env: map[string]string{"TOKEN": "x"}},
			{Name: "zeta", Command: "zeta-server", Description: "last"},
		},
	}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("settings =\n%+v\nwant\n%+v", settings, want)
	}
}

func TestSettingsErrorsNameTheFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"unknown top-level field", `{"permission": {}}`, `unknown field "permission"`},
		{"unknown permissions field", `{"permissions": {"ask": []}}`, `unknown field "ask"`},
		{"unknown hook event", `{"hooks": {"after_prompt": []}}`, `unknown field "after_prompt"`},
		{"unknown hook field", `{"hooks": {"before_tool": [{"command": "x", "matcher": "bash"}]}}`, `unknown field "matcher"`},
		{"unknown server field", `{"mcp_servers": {"a": {"command": "x", "cwd": "/"}}}`, `unknown field "cwd"`},
		{"invalid JSON", `{"permissions": `, "parse"},
		{"trailing data", `{} {}`, "data after the JSON value"},
		{"empty hook command", `{"hooks": {"after_tool": [{"command": " "}]}}`, "hooks.after_tool[0]: command is empty"},
		{"prompt hook with tools", `{"hooks": {"prompt": [{"command": "x", "tools": ["bash"]}]}}`, "prompt hooks do not take tools"},
		{"empty tool name in filter", `{"hooks": {"before_tool": [{"command": "x", "tools": [""]}]}}`, "tools has an empty name"},
		{"server without command", `{"mcp_servers": {"a": {"args": ["x"]}}}`, "mcp_servers.a: command is empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dirs := newLayout(t)
			path := filepath.Join(dirs.projectDir(), settingsFileName)
			writeFile(t, path, test.content)
			_, err := loadSettings(dirs.userDir, dirs.workDir)
			wantErrorContaining(t, err, path, test.want)
		})
	}
}
