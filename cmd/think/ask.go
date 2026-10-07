package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/terminal"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// question is what the model asks the user: a question and 2 to 6 choices.
type question struct {
	Text    string
	Options []string
}

// questionAsker shows a question and returns the answer: one of the options
// or the user's own text. A nil asker means that no user can answer, as in
// run -json, rpc, and eval.
type questionAsker func(ctx context.Context, asked question) (string, error)

const (
	minimumOptions = 2
	maximumOptions = 6
	optionLimit    = 200
	questionLimit  = 1000
)

// askUserTool lets the model ask a multiple-choice question instead of
// guessing. It changes nothing, but it is not read-only: two questions must
// not run in parallel.
func askUserTool(ask questionAsker) tools.Tool {
	return tools.Tool{
		Name: "ask_user",
		Description: "Ask the user one multiple-choice question when a decision is theirs and a wrong guess would waste work, for example between two designs. " +
			"Give 2 to 6 short options; the user can also type a different answer. Do not ask for facts that you can find with tools. " +
			"When no user is present, the result says so; then choose, state your assumption, and continue.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":6}},"required":["question","options"],"additionalProperties":false}`),
		Run: func(ctx context.Context, input json.RawMessage) (tools.Result, error) {
			asked, err := parseQuestion(input)
			if err != nil {
				return tools.Result{Content: "invalid input: " + err.Error(), IsError: true}, nil
			}
			if ask == nil {
				return tools.Result{Content: "No user can answer now: this run is not interactive. Choose the most reasonable option, state your assumption in your reply, and continue.", IsError: true}, nil
			}
			answer, err := ask(ctx, asked)
			if err != nil {
				return tools.Result{}, err
			}
			for _, option := range asked.Options {
				if answer == option {
					return tools.Result{Content: "The user chose: " + option}, nil
				}
			}
			return tools.Result{Content: "The user answered in their own words: " + answer}, nil
		},
	}
}

func parseQuestion(input json.RawMessage) (question, error) {
	var arguments struct {
		Question *string  `json:"question"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal(input, &arguments); err != nil {
		return question{}, err
	}
	if arguments.Question == nil || strings.TrimSpace(*arguments.Question) == "" {
		return question{}, fmt.Errorf("question is required")
	}
	if len(*arguments.Question) > questionLimit {
		return question{}, fmt.Errorf("question must be at most %d bytes", questionLimit)
	}
	if len(arguments.Options) < minimumOptions || len(arguments.Options) > maximumOptions {
		return question{}, fmt.Errorf("give %d to %d options, not %d", minimumOptions, maximumOptions, len(arguments.Options))
	}
	options := make([]string, len(arguments.Options))
	for index, option := range arguments.Options {
		option = strings.TrimSpace(option)
		if option == "" || strings.ContainsAny(option, "\r\n") || len(option) > optionLimit {
			return question{}, fmt.Errorf("option %d must be one non-empty line of at most %d bytes", index+1, optionLimit)
		}
		for _, earlier := range options[:index] {
			if earlier == option {
				return question{}, fmt.Errorf("option %q is repeated", option)
			}
		}
		options[index] = option
	}
	return question{Text: strings.TrimSpace(*arguments.Question), Options: options}, nil
}

// formatQuestion shows a question with numbered options.
func formatQuestion(asked question, theme presentation.Theme) string {
	var out strings.Builder
	out.WriteString("\n" + theme.Accent("?") + " " + theme.Bold(theme.Ink(presentation.Safe(asked.Text))))
	for index, option := range asked.Options {
		fmt.Fprintf(&out, "\n  %s %s", theme.Key(strconv.Itoa(index+1)), presentation.Safe(option))
	}
	return out.String()
}

// pickAnswer maps the user's input to an option number, or keeps their own
// text. Empty input is not an answer.
func pickAnswer(input string, options []string) (string, bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", false
	}
	if number, err := strconv.Atoi(input); err == nil {
		if number >= 1 && number <= len(options) {
			return options[number-1], true
		}
		return "", false
	}
	return input, true
}

// screenAsker asks in the terminal interface. The answer goes into the
// composer: a number picks an option, other text is the user's own answer.
func screenAsker(screen *terminal.Screen, output io.Writer, theme presentation.Theme) questionAsker {
	return func(ctx context.Context, asked question) (string, error) {
		fmt.Fprintln(output, formatQuestion(asked, theme))
		screen.Notify("Inkling asks: " + asked.Text)
		for {
			input, err := screen.Prompt(ctx, fmt.Sprintf("Answer with 1-%d or your own words", len(asked.Options)))
			if err != nil {
				return "", err
			}
			if answer, ok := pickAnswer(input, asked.Options); ok {
				fmt.Fprintln(output, "  "+theme.Graphite("answer: ")+presentation.Safe(answer))
				return answer, nil
			}
		}
	}
}

// lineAsker asks in plain mode, one line for each answer.
func lineAsker(input *lineInput, output io.Writer) questionAsker {
	return func(ctx context.Context, asked question) (string, error) {
		fmt.Fprintln(output, formatQuestion(asked, presentation.Theme{}))
		for {
			fmt.Fprintf(output, "Answer with 1-%d or your own words: ", len(asked.Options))
			line, err := input.next(ctx)
			if err != nil {
				return "", err
			}
			if answer, ok := pickAnswer(line, asked.Options); ok {
				return answer, nil
			}
		}
	}
}
