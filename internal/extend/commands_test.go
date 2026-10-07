package extend

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCommandsProjectOverridesUserAndSortByName(t *testing.T) {
	dirs := newLayout(t)
	userCommands := filepath.Join(dirs.userDir, commandsDirName)
	projectCommands := filepath.Join(dirs.projectDir(), commandsDirName)
	writeFile(t, filepath.Join(userCommands, "review.md"), "Review as the user.")
	writeFile(t, filepath.Join(userCommands, "zz-last.md"), "---\ndescription: Last one\n---\nLast $ARGUMENTS\n")
	writeFile(t, filepath.Join(userCommands, "notes.txt"), "not a command")
	writeFile(t, filepath.Join(projectCommands, "review.md"), "---\r\ndescription: \"Review the diff\"\r\nread-only: true\r\n---\r\n\r\nReview $ARGUMENTS carefully.\r\n")
	writeFile(t, filepath.Join(projectCommands, "a_fix-1.md"), "---\nread-only: false\n---\nFix it.")

	commands, err := loadCommands(dirs.userDir, dirs.workDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Command{
		{Name: "a_fix-1", Template: "Fix it.", Source: filepath.Join(projectCommands, "a_fix-1.md")},
		{Name: "review", Description: "Review the diff", Template: "Review $ARGUMENTS carefully.", Source: filepath.Join(projectCommands, "review.md"), ReadOnly: true},
		{Name: "zz-last", Description: "Last one", Template: "Last $ARGUMENTS", Source: filepath.Join(userCommands, "zz-last.md")},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands =\n%+v\nwant\n%+v", commands, want)
	}
}

func TestCommandFileErrors(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		content  string
		want     string
	}{
		{"unknown front matter key", "deploy.md", "---\nmodel: fast\n---\nDeploy.", `unknown front matter key "model"`},
		{"bad read-only value", "deploy.md", "---\nread-only: yes\n---\nDeploy.", `read-only must be true or false, not "yes"`},
		{"front matter not closed", "deploy.md", "---\ndescription: x\nread-only: true\n", "no closing ---"},
		{"line without colon", "deploy.md", "---\njust text\n---\nDeploy.", "is not \"key: value\""},
		{"duplicate key", "deploy.md", "---\ndescription: a\ndescription: b\n---\nDeploy.", "occurs two times"},
		{"empty template", "deploy.md", "---\ndescription: nothing\n---\n  \n", "no template text"},
		{"upper case name", "Deploy.md", "Deploy.", "must match [a-z0-9][a-z0-9_-]*"},
		{"name starts with dash", "-deploy.md", "Deploy.", "must match"},
		{"name with space", "my deploy.md", "Deploy.", "must match"},
		{"built-in name", "plan.md", "My plan.", "taken by a built-in command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dirs := newLayout(t)
			path := filepath.Join(dirs.userDir, commandsDirName, test.fileName)
			writeFile(t, path, test.content)
			_, err := loadCommands(dirs.userDir, dirs.workDir)
			wantErrorContaining(t, err, path, test.want)
		})
	}
}

func TestCommandExpand(t *testing.T) {
	tests := []struct {
		name      string
		template  string
		arguments string
		want      string
	}{
		{"every placeholder replaced", "Fix $ARGUMENTS, then test $ARGUMENTS.", "parser", "Fix parser, then test parser."},
		{"placeholder with empty arguments", "Fix $ARGUMENTS.", "", "Fix ."},
		{"no placeholder appends arguments", "Review the diff.", "focus on errors", "Review the diff.\n\nfocus on errors"},
		{"no placeholder and no arguments", "Review the diff.", "", "Review the diff."},
		{"arguments with a placeholder are not expanded again", "Do $ARGUMENTS", "$ARGUMENTS x", "Do $ARGUMENTS x"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := (Command{Template: test.template}).Expand(test.arguments); got != test.want {
				t.Fatalf("Expand(%q) = %q, want %q", test.arguments, got, test.want)
			}
		})
	}
}

func TestBuiltinPlanCommand(t *testing.T) {
	builtins := BuiltinCommands()
	if len(builtins) != 1 {
		t.Fatalf("built-in commands = %d, want 1", len(builtins))
	}
	plan := builtins[0]
	if plan.Name != "plan" || !plan.ReadOnly || plan.Source != BuiltinSource || plan.Description == "" {
		t.Fatalf("plan = %+v", plan)
	}
	expanded := plan.Expand("add a cache to the client")
	for _, part := range []string{"add a cache to the client", "read-only tools", "Do not change files", "numbered plan"} {
		if !strings.Contains(expanded, part) {
			t.Fatalf("expanded plan %q does not contain %q", expanded, part)
		}
	}
	if strings.Contains(expanded, argumentsPlaceholder) {
		t.Fatalf("expanded plan still has the placeholder: %q", expanded)
	}
}
