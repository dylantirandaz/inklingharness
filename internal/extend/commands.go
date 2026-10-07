package extend

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	commandsDirName      = "commands"
	argumentsPlaceholder = "$ARGUMENTS"
)

// BuiltinSource is the Source of a command that needs no file.
const BuiltinSource = "built-in"

// Command is a named prompt template that the user starts with /name.
type Command struct {
	Name, Description, Template string
	// Source is the file path, or "built-in".
	Source string
	// ReadOnly asks the agent to run the prompt with read-only tools.
	ReadOnly bool
}

// Expand puts arguments in place of every $ARGUMENTS. A template without the
// placeholder gets the arguments as a last paragraph, so they are not lost.
func (c Command) Expand(arguments string) string {
	if strings.Contains(c.Template, argumentsPlaceholder) {
		return strings.ReplaceAll(c.Template, argumentsPlaceholder, arguments)
	}
	if arguments == "" {
		return c.Template
	}
	return c.Template + "\n\n" + arguments
}

// BuiltinCommands returns the commands that need no file.
func BuiltinCommands() []Command {
	return []Command{{
		Name:        "plan",
		Description: "Investigate with read-only tools and reply with a numbered plan. No file changes.",
		Template: "Make a plan for this task: " + argumentsPlaceholder + "\n\n" +
			"Investigate the code with read-only tools only. Do not change files. " +
			"Do not run commands that change state. " +
			"Then reply with a numbered plan. Give one step on each line, and name the files that each step changes. " +
			"Write down the risks and the open questions at the end.",
		Source:   BuiltinSource,
		ReadOnly: true,
	}}
}

// LoadCommands reads commands/*.md from the user directory and from
// <workDir>/.inkling. The result is sorted by Name.
func LoadCommands(workDir string) ([]Command, error) {
	userDir, err := UserDir()
	if err != nil {
		return nil, err
	}
	return loadCommands(userDir, workDir)
}

func loadCommands(userDir, workDir string) ([]Command, error) {
	builtins := BuiltinCommands()
	byName := map[string]Command{}
	for _, dir := range layerDirs(userDir, workDir) {
		commandsDir := filepath.Join(dir, commandsDirName)
		entries, err := readDirIfExists(commandsDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			baseName, isMarkdown := strings.CutSuffix(entry.Name(), ".md")
			if entry.IsDir() || !isMarkdown {
				continue
			}
			path := filepath.Join(commandsDir, entry.Name())
			if !isValidName(baseName) {
				return nil, fmt.Errorf("extend: %s: command name %q must match [a-z0-9][a-z0-9_-]*", path, baseName)
			}
			if slices.ContainsFunc(builtins, func(builtin Command) bool { return builtin.Name == baseName }) {
				return nil, fmt.Errorf("extend: %s: command name %q is taken by a built-in command", path, baseName)
			}
			command, err := readCommand(path, baseName)
			if err != nil {
				return nil, err
			}
			byName[baseName] = command
		}
	}
	commands := make([]Command, 0, len(byName))
	for _, command := range byName {
		commands = append(commands, command)
	}
	slices.SortFunc(commands, func(left, right Command) int {
		return strings.Compare(left.Name, right.Name)
	})
	return commands, nil
}

func readCommand(path, name string) (Command, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Command{}, fmt.Errorf("extend: read %s: %w", path, err)
	}
	command := Command{Name: name, Source: path}
	body := content
	firstLine, _, _ := bytes.Cut(content, []byte("\n"))
	if string(bytes.TrimSuffix(firstLine, []byte("\r"))) == frontMatterDelimiter {
		reader := bufio.NewReader(bytes.NewReader(content))
		fields, err := readFrontMatter(reader, "description", "read-only")
		if err != nil {
			return Command{}, fmt.Errorf("extend: %s: %w", path, err)
		}
		command.Description = fields["description"]
		switch readOnly, present := fields["read-only"]; {
		case !present, readOnly == "false":
			command.ReadOnly = false
		case readOnly == "true":
			command.ReadOnly = true
		default:
			return Command{}, fmt.Errorf("extend: %s: read-only must be true or false, not %q", path, readOnly)
		}
		if body, err = io.ReadAll(reader); err != nil {
			return Command{}, fmt.Errorf("extend: read %s: %w", path, err)
		}
	}
	command.Template = strings.TrimSpace(string(body))
	if command.Template == "" {
		return Command{}, fmt.Errorf("extend: %s: command has no template text", path)
	}
	return command, nil
}
