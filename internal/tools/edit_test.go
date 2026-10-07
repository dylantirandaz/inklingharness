package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEditFileReportsLinesAndClosestText(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.go")
	original := "package user\n\nfunc Rename(name string) error {\n\tif name == \"\" {\n\t\treturn errEmpty\n\t}\n\treturn nil\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := lookup(t, root, "edit_file")

	result := runTool(t, edit, `{"path":"user.go","old_string":"\treturn nil\n","new_string":"\tlog(name)\n\treturn nil\n"}`)
	if result.IsError || result.Content != "edited user.go: now lines 7-8" {
		t.Fatalf("edit = %+v", result)
	}
	result = runTool(t, edit, `{"path":"user.go","old_string":"\tlog(name)\n","new_string":""}`)
	if result.IsError || result.Content != "edited user.go: removed text at line 7" {
		t.Fatalf("delete = %+v", result)
	}

	// Spaces instead of a tab: the same lines apart from indentation.
	result = runTool(t, edit, `{"path":"user.go","old_string":"    if name == \"\" {\n        return errEmpty\n","new_string":"x"}`)
	want := "old_string not found in user.go. Current text matching except for indentation or trailing spaces, lines 2-7:\n" +
		"2: \n3: func Rename(name string) error {\n4: \tif name == \"\" {\n5: \t\treturn errEmpty\n6: \t}\n7: \treturn nil"
	if !result.IsError || result.Content != want {
		t.Fatalf("whitespace miss =\n%s\nwant\n%s", result.Content, want)
	}
	// Text changed by an earlier edit: the closest lines are shown.
	result = runTool(t, edit, `{"path":"user.go","old_string":"func Rename(name string) error {\n\tif name == \"none\" {\n","new_string":"x"}`)
	if !result.IsError || result.Content != "old_string not found in user.go. Current text closest, lines 1-6:\n1: package user\n2: \n3: func Rename(name string) error {\n4: \tif name == \"\" {\n5: \t\treturn errEmpty\n6: \t}" {
		t.Fatalf("changed text miss =\n%s", result.Content)
	}
	result = runTool(t, edit, `{"path":"user.go","old_string":"nothing like it","new_string":"x"}`)
	if result.Content != "old_string not found in user.go; no line of old_string matches the current file" {
		t.Fatalf("no match = %q", result.Content)
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatalf("failed edits changed the file: %q", data)
	}
}
