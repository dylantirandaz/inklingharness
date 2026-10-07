package fixture

import (
	"strings"
	"testing"
)

func TestNameRules(t *testing.T) {
	cases := map[string]string{
		"ada":                   "",
		"  ":                    "name is empty",
		strings.Repeat("x", 32): "",
		strings.Repeat("x", 33): "name is longer than 32 bytes",
		"a\tb":                  "name contains a control character",
	}
	for name, want := range cases {
		_, createErr := CreateUser(name, "a@b")
		original := User{Name: "old", Email: "a@b"}
		renamed, renameErr := RenameUser(original, name)
		for label, err := range map[string]error{"CreateUser": createErr, "RenameUser": renameErr} {
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != want {
				t.Errorf("%s(%q) error = %q, want %q", label, name, got, want)
			}
		}
		if want == "" && (renamed.Name != name || renamed.Email != "a@b" || original.Name != "old") {
			t.Errorf("RenameUser(%q) = %+v, original %+v", name, renamed, original)
		}
	}
	if _, err := CreateUser("ada", "nope"); err == nil || err.Error() != "email has no @" {
		t.Errorf("CreateUser with bad email error = %v", err)
	}
}
