package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func TestAskUserTool(t *testing.T) {
	var asked question
	answer := ""
	ask := func(_ context.Context, q question) (string, error) {
		asked = q
		return answer, nil
	}
	run := func(input string, ask questionAsker) (string, bool) {
		result, err := askUserTool(ask).Run(context.Background(), json.RawMessage(input))
		if err != nil {
			t.Fatal(err)
		}
		return result.Content, result.IsError
	}
	answer = "Use a map"
	if content, failed := run(`{"question":" Which store? ","options":["Use a map"," Use a slice "]}`, ask); failed || content != "The user chose: Use a map" {
		t.Fatalf("option answer = %q %t", content, failed)
	}
	if asked.Text != "Which store?" || asked.Options[1] != "Use a slice" {
		t.Fatalf("asked = %+v", asked)
	}
	answer = "neither, use sqlite"
	if content, failed := run(`{"question":"Which store?","options":["map","slice"]}`, ask); failed || content != "The user answered in their own words: neither, use sqlite" {
		t.Fatalf("own answer = %q %t", content, failed)
	}
	// Without a user the model does not get the tool at all.
	ext := &extensions{workDir: t.TempDir()}
	empty, err := tools.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ask  questionAsker
		want bool
	}{{nil, false}, {ask, true}} {
		set, err := ext.toolSet(empty, test.ask)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := set.Lookup("ask_user"); found != test.want {
			t.Fatalf("ask_user offered = %t with asker %t", found, test.ask != nil)
		}
	}
	for _, input := range []string{
		`{"options":["a","b"]}`, `{"question":"","options":["a","b"]}`, `{"question":"q","options":["a"]}`,
		`{"question":"q","options":["a","b","c","d","e","f","g"]}`, `{"question":"q","options":["a","a"]}`,
		`{"question":"q","options":["a","two\nlines"]}`, `{"question":"q","options":["a",""]}`, `[]`,
	} {
		if content, failed := run(input, ask); !failed || !strings.HasPrefix(content, "invalid input") {
			t.Fatalf("%s accepted: %q", input, content)
		}
	}
}

func TestPickAnswer(t *testing.T) {
	options := []string{"first", "second", "third"}
	for input, want := range map[string]string{"1": "first", " 3 ": "third", "my own idea": "my own idea", "2nd": "2nd"} {
		if got, ok := pickAnswer(input, options); !ok || got != want {
			t.Fatalf("pickAnswer(%q) = %q %t", input, got, ok)
		}
	}
	for _, input := range []string{"", "  ", "0", "4", "-1"} {
		if got, ok := pickAnswer(input, options); ok {
			t.Fatalf("pickAnswer(%q) accepted %q", input, got)
		}
	}
}
