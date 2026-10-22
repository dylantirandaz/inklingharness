package main

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

var webToolNames = []string{"web_search", "web_fetch"}

// -no-web must take the web tools out of the request and out of the prompt
// together. A prompt that tells the model to use a tool that is missing costs
// turns, and a tool that the prompt never names is used less.
func TestNoWebRemovesTheWebToolsAndTheirPromptLine(t *testing.T) {
	for _, arguments := range [][]string{nil, {"-no-web"}} {
		flags := flag.NewFlagSet("think run", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		o := bindOptions(flags)
		if err := flags.Parse(arguments); err != nil {
			t.Fatal(err)
		}
		config, err := o.agentConfig()
		if err != nil {
			t.Fatal(err)
		}
		empty, err := tools.NewSet()
		if err != nil {
			t.Fatal(err)
		}
		set, err := (&extensions{workDir: t.TempDir(), noWeb: o.noWeb}).toolSet(empty, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range webToolNames {
			_, offered := set.Lookup(name)
			if offered == o.noWeb {
				t.Errorf("arguments %v: %s offered = %t", arguments, name, offered)
			}
			if mentioned := strings.Contains(config.System, name); mentioned != offered {
				t.Errorf("arguments %v: the prompt names %s = %t, but the tool is offered = %t", arguments, name, mentioned, offered)
			}
		}
		if _, found := set.Lookup("remember"); !found {
			t.Errorf("arguments %v: -no-web must keep the other tools", arguments)
		}
	}
}

// think eval builds its tools apart from a session, so it needs the same check.
func TestEvalSetupHonorsNoWeb(t *testing.T) {
	for _, noWeb := range []bool{false, true} {
		outputs := t.TempDir()
		jobs := tools.NewJobs(outputs)
		var config agent.Config
		set, err := evalSetup(noWeb)(t.TempDir(), outputs, jobs, &config)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range webToolNames {
			if _, offered := set.Lookup(name); offered == noWeb {
				t.Errorf("noWeb %t: %s offered = %t", noWeb, name, offered)
			}
		}
		if err := jobs.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
