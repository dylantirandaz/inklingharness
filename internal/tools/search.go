package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const maxGrepFileBytes = 2 * 1024 * 1024
const maxGrepLineBytes = 16 * 1024

type searchOutput struct {
	lines     []string
	bytes     int
	limit     int
	truncated bool
}

func (o *searchOutput) add(line string) bool {
	if len(o.lines) >= o.limit || len(line)+1 > outputBodyBytes-o.bytes {
		o.truncated = true
		return false
	}
	o.lines = append(o.lines, line)
	o.bytes += len(line) + 1
	return true
}

func (o *searchOutput) result(sorted bool, notice string) Result {
	if sorted {
		sort.Strings(o.lines)
	}
	text := strings.Join(o.lines, "\n")
	if len(o.lines) == 0 {
		text = "[no results]"
	}
	if o.truncated {
		text += "\n[truncated: result or 256 KiB output limit]"
	}
	if notice != "" {
		text += "\n" + notice
	}
	return Result{Content: text}
}

// Patterns use slash-separated path segments; ** matches zero or more segments.
func parseGlob(pattern string) ([]string, error) {
	if pattern == "" || strings.HasPrefix(pattern, "/") {
		return nil, errors.New("glob pattern must be a nonempty relative path")
	}
	parts := strings.Split(pattern, "/")
	for _, part := range parts {
		if part == "" || part == ".." {
			return nil, errors.New("glob pattern cannot contain empty or .. segments")
		}
		if _, err := path.Match(part, ""); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

func matchGlob(pattern []string, name string) bool {
	type position struct {
		pattern   int
		remaining string
	}
	var previous position
	var backtrack *position
	index := 0
	for name != "" {
		if index < len(pattern) && pattern[index] == "**" {
			previous = position{pattern: index + 1, remaining: name}
			backtrack = &previous
			index++
			continue
		}
		part, rest, _ := strings.Cut(name, "/")
		if index < len(pattern) {
			matched, _ := path.Match(pattern[index], part)
			if matched {
				index++
				name = rest
				continue
			}
		}
		if backtrack == nil {
			return false
		}
		_, rest, _ = strings.Cut(backtrack.remaining, "/")
		backtrack.remaining = rest
		name, index = rest, backtrack.pattern
	}
	for index < len(pattern) && pattern[index] == "**" {
		index++
	}
	return index == len(pattern)
}

// WalkDir does not recurse into symlinks; only regular files are searched.
func walkFiles(ctx context.Context, base string, visit func(string, string, fs.DirEntry) error) error {
	return filepath.WalkDir(base, func(filename string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(base, filename)
		if err != nil {
			return err
		}
		if relative == "." {
			relative = filepath.Base(filename)
		}
		return visit(filename, filepath.ToSlash(relative), entry)
	})
}

func globTool(root string) Tool {
	return Tool{
		Name:        "glob",
		Description: "Find files by glob pattern; ** matches any directories. Returns sorted relative paths. Skips .git, node_modules, and symlinks.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"limit":{"type":"integer","minimum":1}},"required":["pattern"]}`),
		ReadOnly:    true,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			arguments := struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Limit   int    `json:"limit"`
			}{Path: ".", Limit: 1000}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if arguments.Limit < 1 {
				return invalidInput(errors.New("limit must be positive")), nil
			}
			pattern, err := parseGlob(arguments.Pattern)
			if err != nil {
				return invalidInput(err), nil
			}
			output := searchOutput{limit: arguments.Limit}
			err = walkFiles(ctx, resolvePath(root, arguments.Path), func(filename, relative string, entry fs.DirEntry) error {
				if matchGlob(pattern, relative) && !output.add(relative) {
					return filepath.SkipAll
				}
				return nil
			})
			if err != nil {
				return fileFailure(err)
			}
			return output.result(true, ""), nil
		},
	}
}

func grepTool(root string) Tool {
	return Tool{
		Name:        "grep",
		Description: "Search file contents with a Go regular expression. Returns path:line:text. include is a file glob. Skips .git, node_modules, binary files, and files over 2 MiB.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"include":{"type":"string"},"limit":{"type":"integer","minimum":1},"case_sensitive":{"type":"boolean"}},"required":["pattern"]}`),
		ReadOnly:    true,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			arguments := struct {
				Pattern       *string `json:"pattern"`
				Path          string  `json:"path"`
				Include       *string `json:"include"`
				Limit         int     `json:"limit"`
				CaseSensitive bool    `json:"case_sensitive"`
			}{Path: ".", Limit: 100, CaseSensitive: true}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if arguments.Pattern == nil || arguments.Limit < 1 {
				return invalidInput(errors.New("pattern is required and limit must be positive")), nil
			}
			pattern := *arguments.Pattern
			if !arguments.CaseSensitive {
				pattern = "(?i)" + pattern
			}
			expression, err := regexp.Compile(pattern)
			if err != nil {
				return invalidInput(err), nil
			}
			var include []string
			if arguments.Include != nil {
				include, err = parseGlob(*arguments.Include)
				if err != nil {
					return invalidInput(err), nil
				}
			}
			output := searchOutput{limit: arguments.Limit}
			largeFiles, binaryFiles, longLines := 0, 0, 0
			err = walkFiles(ctx, resolvePath(root, arguments.Path), func(filename, relative string, entry fs.DirEntry) error {
				name := relative
				if len(include) == 1 {
					name = path.Base(relative)
				}
				if include != nil && !matchGlob(include, name) {
					return nil
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Size() > maxGrepFileBytes {
					largeFiles++
					return nil
				}
				file, err := os.Open(filename)
				if err != nil {
					return err
				}
				content, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: file}, maxGrepFileBytes+1))
				file.Close()
				if err != nil {
					return err
				}
				if len(content) > maxGrepFileBytes {
					largeFiles++
					return nil
				}
				if bytes.IndexByte(content, 0) >= 0 {
					binaryFiles++
					return nil
				}
				reader := bufio.NewReader(bytes.NewReader(content))
				for number := 1; ; number++ {
					text, truncated, err := boundedLine(ctx, reader, maxGrepLineBytes)
					if errors.Is(err, io.EOF) {
						return nil
					}
					if err != nil {
						return err
					}
					if truncated {
						longLines++
						continue
					}
					if expression.MatchString(text) && !output.add(fmt.Sprintf("%s:%d:%s", relative, number, text)) {
						return filepath.SkipAll
					}
				}
			})
			if err != nil {
				return fileFailure(err)
			}
			notice := ""
			if largeFiles+binaryFiles+longLines > 0 {
				notice = fmt.Sprintf("[skipped: %d files over 2 MiB, %d binary files (NUL), %d lines over 16 KiB]", largeFiles, binaryFiles, longLines)
			}
			return output.result(false, notice), nil
		},
	}
}
