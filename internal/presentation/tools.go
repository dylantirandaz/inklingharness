package presentation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ToolView separates a compact activity label from the complete approval text.
type ToolView struct {
	Title   string
	Details string
}

type toolInput struct {
	Path           *string         `json:"path"`
	Offset         *int            `json:"offset"`
	Limit          *int            `json:"limit"`
	Content        *string         `json:"content"`
	OldString      *string         `json:"old_string"`
	NewString      *string         `json:"new_string"`
	Pattern        *string         `json:"pattern"`
	Include        *string         `json:"include"`
	CaseSensitive  *bool           `json:"case_sensitive"`
	Command        *string         `json:"command"`
	TimeoutSeconds *int            `json:"timeout_seconds"`
	Prompt         *string         `json:"prompt"`
	Action         *string         `json:"action"`
	ID             *int            `json:"id"`
	Server         *string         `json:"server"`
	Tool           *string         `json:"tool"`
	Arguments      json.RawMessage `json:"arguments"`
	URL            *string         `json:"url"`
	Query          *string         `json:"query"`
	Count          *int            `json:"count"`
	MaxChars       *int            `json:"max_chars"`
	Fact           *string         `json:"fact"`
	Question       *string         `json:"question"`
	Options        *[]string       `json:"options"`
}

// DescribeTool formats only declared tools. Details never abbreviates a command,
// file body, replacement, or task prompt; it does not inspect the filesystem.
func DescribeTool(name string, input json.RawMessage, theme Theme) (ToolView, error) {
	return describeTool(name, input, theme, true)
}

// ToolTitle shares tool validation and labels without formatting approval
// bodies. The title is plain text, so callers can style or measure it.
func ToolTitle(name string, input json.RawMessage) (string, error) {
	view, err := describeTool(name, input, Theme{}, false)
	return view.Title, err
}

func describeTool(name string, input json.RawMessage, theme Theme, details bool) (ToolView, error) {
	input = bytes.TrimSpace(input)
	if len(input) == 0 || input[0] != '{' {
		return ToolView{}, errors.New("tool input must be a JSON object")
	}
	switch name {
	case "read_file", "write_file", "edit_file", "glob", "grep", "bash", "bash_job", "task", "mcp_list", "mcp_call", "web_search", "web_fetch", "remember", "ask_user":
	default:
		return describeOtherTool(name, input, details)
	}
	var args toolInput
	if err := json.Unmarshal(input, &args); err != nil {
		return ToolView{}, fmt.Errorf("invalid %s input: %w", name, err)
	}
	var view ToolView
	switch name {
	case "read_file":
		if err := requiredText(args.Path, "path", false); err != nil {
			return ToolView{}, err
		}
		offset, err := positive(args.Offset, 1, "offset")
		if err != nil {
			return ToolView{}, err
		}
		limit, err := positive(args.Limit, 2000, "limit")
		if err != nil {
			return ToolView{}, err
		}
		view.Title = "Read " + brief(*args.Path)
		if details {
			view.Details = fmt.Sprintf("Read %s\nStart line: %d\nLine limit: %d", singleLine(*args.Path), offset, limit)
		}
	case "write_file":
		if err := requiredText(args.Path, "path", false); err != nil {
			return ToolView{}, err
		}
		if err := requiredText(args.Content, "content", true); err != nil {
			return ToolView{}, err
		}
		view.Title = "Write " + brief(*args.Path)
		if details {
			view.Details = "Write or replace " + singleLine(*args.Path) + "\n" + diffLines(*args.Content, "+ ", theme.Green, theme)
		}
	case "edit_file":
		if err := requiredText(args.Path, "path", false); err != nil {
			return ToolView{}, err
		}
		if err := requiredText(args.OldString, "old_string", false); err != nil {
			return ToolView{}, err
		}
		if err := requiredText(args.NewString, "new_string", true); err != nil {
			return ToolView{}, err
		}
		view.Title = "Edit " + brief(*args.Path)
		if details {
			view.Details = "Replace one exact occurrence in " + singleLine(*args.Path) + "\n" +
				diffLines(*args.OldString, "- ", theme.Red, theme) + "\n" + diffLines(*args.NewString, "+ ", theme.Green, theme)
		}
	case "glob", "grep":
		path := "."
		if args.Path != nil {
			path = *args.Path
		}
		defaultLimit := 1000
		if name == "grep" {
			defaultLimit = 100
		}
		limit, err := positive(args.Limit, defaultLimit, "limit")
		if err != nil {
			return ToolView{}, err
		}
		if err := requiredText(args.Pattern, "pattern", false); err != nil {
			return ToolView{}, err
		}
		label := "Find files "
		if name == "grep" {
			label = "Search "
		}
		view.Title = label + brief(*args.Pattern) + " in " + brief(path)
		if details {
			view.Details = fmt.Sprintf("Pattern: %s\nDirectory: %s\nResult limit: %d", singleLine(*args.Pattern), singleLine(path), limit)
		}
		if name == "grep" && details {
			caseSensitive := true
			if args.CaseSensitive != nil {
				caseSensitive = *args.CaseSensitive
			}
			view.Details += fmt.Sprintf("\nCase sensitive: %t", caseSensitive)
			if args.Include != nil {
				view.Details += "\nInclude: " + singleLine(*args.Include)
			}
		}
	case "bash":
		if err := requiredText(args.Command, "command", false); err != nil {
			return ToolView{}, err
		}
		timeout, err := positive(args.TimeoutSeconds, 120, "timeout_seconds")
		if err != nil {
			return ToolView{}, err
		}
		if timeout > 600 {
			return ToolView{}, errors.New("timeout_seconds must not exceed 600")
		}
		view.Title = "Run " + brief(*args.Command)
		if details {
			view.Details = fmt.Sprintf("Bash · timeout %ds\n%s", timeout, Safe(*args.Command))
		}
	case "task":
		if err := requiredText(args.Prompt, "prompt", false); err != nil {
			return ToolView{}, err
		}
		view.Title = "Delegate " + brief(*args.Prompt)
		if details {
			view.Details = "Task prompt\n" + Safe(*args.Prompt)
		}
	case "bash_job":
		if args.Action == nil {
			return ToolView{}, errors.New("action is required")
		}
		switch *args.Action {
		case "list":
			view.Title = "Job list"
		case "output", "kill":
			if args.ID == nil {
				return ToolView{}, fmt.Errorf("action %s needs an id", *args.Action)
			}
			view.Title = fmt.Sprintf("Job %s %d", *args.Action, *args.ID)
		default:
			return ToolView{}, fmt.Errorf("unknown job action %q", singleLine(*args.Action))
		}
		if details {
			view.Details = view.Title + " (a background job that an approved bash call started)"
		}
	case "mcp_list":
		if err := requiredText(args.Server, "server", false); err != nil {
			return ToolView{}, err
		}
		view.Title = "MCP tools of " + brief(*args.Server)
		if details {
			view.Details = "List the tools of MCP server " + singleLine(*args.Server) + "; this starts the server once."
		}
	case "mcp_call":
		if err := requiredText(args.Server, "server", false); err != nil {
			return ToolView{}, err
		}
		if err := requiredText(args.Tool, "tool", false); err != nil {
			return ToolView{}, err
		}
		view.Title = "MCP " + brief(*args.Server+"/"+*args.Tool)
		if details {
			view.Details = "Call " + singleLine(*args.Tool) + " on MCP server " + singleLine(*args.Server) + "\nArguments\n" + indentedJSON(args.Arguments)
		}
	case "web_search":
		if err := requiredText(args.Query, "query", false); err != nil {
			return ToolView{}, err
		}
		count, err := positive(args.Count, 5, "count")
		if err != nil {
			return ToolView{}, err
		}
		if count > 10 {
			return ToolView{}, errors.New("count must not exceed 10")
		}
		view.Title = "Search web " + brief(*args.Query)
		if details {
			view.Details = fmt.Sprintf("Query: %s\nResult limit: %d\nThe query goes to Exa; results go to the model provider.", Safe(*args.Query), count)
		}
	case "web_fetch":
		if err := requiredText(args.URL, "url", false); err != nil {
			return ToolView{}, err
		}
		view.Title = "Fetch " + brief(*args.URL)
		if details {
			view.Details = "Fetch " + singleLine(*args.URL) + "\nThe page text goes to the model provider."
		}
	case "remember":
		if err := requiredText(args.Fact, "fact", false); err != nil {
			return ToolView{}, err
		}
		view.Title = "Remember " + brief(*args.Fact)
		if details {
			view.Details = "Add to .inkling/memory.md\n" + Safe(*args.Fact)
		}
	case "ask_user":
		if err := requiredText(args.Question, "question", false); err != nil {
			return ToolView{}, err
		}
		view.Title = "Ask " + brief(*args.Question)
		if details {
			view.Details = Safe(*args.Question)
			if args.Options != nil {
				for index, option := range *args.Options {
					view.Details += fmt.Sprintf("\n%d. %s", index+1, singleLine(option))
				}
			}
		}
	}
	return view, nil
}

// describeOtherTool describes a tool that this package does not know, such
// as a custom tool from the user's files: its name and its full input.
func describeOtherTool(name string, input json.RawMessage, details bool) (ToolView, error) {
	if !json.Valid(input) {
		return ToolView{}, fmt.Errorf("invalid %s input", singleLine(name))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, input); err != nil {
		return ToolView{}, fmt.Errorf("invalid %s input: %w", singleLine(name), err)
	}
	view := ToolView{Title: brief(name + " " + compact.String())}
	if details {
		view.Details = "Tool " + singleLine(name) + "\nInput\n" + indentedJSON(input)
	}
	return view, nil
}

// indentedJSON shows JSON for a person; Safe removes terminal controls that
// a string inside it could hold.
func indentedJSON(value json.RawMessage) string {
	if len(bytes.TrimSpace(value)) == 0 {
		return "{}"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, value, "", "  "); err != nil {
		return Safe(string(value))
	}
	return Safe(out.String())
}

// Approval includes the complete safe command or change, never a preview. An
// amber rule marks the request. Details pass through Safe again, which
// removes their styles; diff prefixes then select the color. The prefixes
// also show the change without color.
func Approval(view ToolView, theme Theme) string {
	bar := ApprovalBar(theme)
	var out strings.Builder
	title := singleLine(view.Title)
	out.WriteString(bar + theme.Amber("Approval needed") + " · " + title)
	details := Safe(view.Details)
	if first, rest, found := strings.Cut(details, "\n"); found && first == title {
		details = rest
	}
	for line := range strings.SplitSeq(details, "\n") {
		out.WriteString("\n" + bar)
		switch {
		case strings.HasPrefix(line, "+ "):
			out.WriteString(theme.Green(line))
		case strings.HasPrefix(line, "- "):
			out.WriteString(theme.Red(line))
		default:
			out.WriteString(line)
		}
	}
	out.WriteString("\n" + bar + theme.Graphite("Full filesystem access · not sandboxed"))
	return out.String()
}

// ApprovalBar starts each row of an approval card and of its outcome.
func ApprovalBar(theme Theme) string {
	return theme.Amber("│") + " "
}

func requiredText(value *string, name string, emptyOK bool) error {
	if value == nil {
		return fmt.Errorf("%s is required and must be a string", name)
	}
	if !emptyOK && *value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	return nil
}

func positive(value *int, fallback int, name string) (int, error) {
	if value == nil {
		return fallback, nil
	}
	if *value < 1 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return *value, nil
}

func diffLines(text, prefix string, style func(string) string, theme Theme) string {
	text = Safe(text)
	if text == "" {
		return style(prefix + "[empty content]")
	}
	var out strings.Builder
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(style(prefix + line))
	}
	if !strings.HasSuffix(text, "\n") {
		out.WriteString("\n")
		out.WriteString(theme.Graphite("\\ No newline at end of content"))
	}
	return out.String()
}
