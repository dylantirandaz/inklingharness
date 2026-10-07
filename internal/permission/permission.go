// Package permission decides from user rules if a tool call can run
// without a question to the user. It only matches text: it does no I/O and
// does not resolve symbolic links.
//
// A rule is "tool" or "tool(pattern)". The bare form covers every call of
// the tool. Only bash, write_file, edit_file, mcp_call and web_fetch accept a
// pattern, because only their inputs have a known meaning. A web_fetch
// pattern matches only the host of the first URL; the fetch can follow
// redirects to other hosts.
package permission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Verdict is the result of a rule check.
type Verdict uint8

const (
	// Ask means that no rule decides; the user must answer.
	Ask Verdict = iota
	// Allow means that the call can run without a question.
	Allow
	// Deny means that the call must not run.
	Deny
)

func (verdict Verdict) String() string {
	switch verdict {
	case Ask:
		return "ask"
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	}
	panic(fmt.Sprintf("permission: unknown verdict %d", uint8(verdict)))
}

// Rules is a parsed set of allow and deny rules. The zero value has no rules.
type Rules struct {
	// Bare rules hold only a tool name, which is also their rule text.
	denyAll, allowAll map[string]struct{}
	commands          ruleLists[commandPattern]
	files             map[string]ruleLists[pathPattern]
	mcpCalls          ruleLists[mcpPattern]
	webFetches        ruleLists[hostPattern]
}

type ruleLists[P any] struct {
	deny, allow []patternRule[P]
}

type patternRule[P any] struct {
	text    string
	pattern P
}

// matcher is a pattern for decoded calls of type C. Deny and allow rules
// can match differently, because a wrong deny match only adds a question,
// but a wrong allow match runs a command that the user did not accept.
type matcher[C any] interface {
	denies(call C) bool
	allows(call C) bool
}

type toolKind uint8

const (
	customTool toolKind = iota
	shellTool
	fileTool
	mcpTool
	webTool
)

func kindOf(tool string) toolKind {
	switch tool {
	case "bash":
		return shellTool
	case "write_file", "edit_file":
		return fileTool
	case "mcp_call":
		return mcpTool
	case "web_fetch":
		return webTool
	default:
		return customTool
	}
}

// Parse reads allow and deny rules. The error names the first bad rule.
func Parse(allow, deny []string) (Rules, error) {
	var rules Rules
	for _, text := range deny {
		if err := rules.add(text, Deny); err != nil {
			return Rules{}, fmt.Errorf("permission: deny rule %q: %w", text, err)
		}
	}
	for _, text := range allow {
		if err := rules.add(text, Allow); err != nil {
			return Rules{}, fmt.Errorf("permission: allow rule %q: %w", text, err)
		}
	}
	return rules, nil
}

func (rules *Rules) add(text string, verdict Verdict) error {
	tool, pattern, hasPattern, err := splitRule(text)
	if err != nil {
		return err
	}
	if !hasPattern {
		switch verdict {
		case Deny:
			rules.denyAll = addTool(rules.denyAll, tool)
		case Allow:
			rules.allowAll = addTool(rules.allowAll, tool)
		case Ask:
			panic("permission: a rule cannot have the verdict ask")
		}
		return nil
	}
	switch kind := kindOf(tool); kind {
	case shellTool:
		parsed, err := parseCommandPattern(pattern)
		if err != nil {
			return err
		}
		rules.commands = rules.commands.with(verdict, patternRule[commandPattern]{text, parsed})
	case fileTool:
		parsed, err := parsePathPattern(pattern)
		if err != nil {
			return err
		}
		if rules.files == nil {
			rules.files = make(map[string]ruleLists[pathPattern], 2)
		}
		rules.files[tool] = rules.files[tool].with(verdict, patternRule[pathPattern]{text, parsed})
	case mcpTool:
		parsed, err := parseMCPPattern(pattern)
		if err != nil {
			return err
		}
		rules.mcpCalls = rules.mcpCalls.with(verdict, patternRule[mcpPattern]{text, parsed})
	case webTool:
		parsed, err := parseHostPattern(pattern)
		if err != nil {
			return err
		}
		rules.webFetches = rules.webFetches.with(verdict, patternRule[hostPattern]{text, parsed})
	case customTool:
		return fmt.Errorf("tool %s does not accept a pattern; use the bare form %s", tool, tool)
	default:
		panic(fmt.Sprintf("permission: unknown tool kind %d", kind))
	}
	return nil
}

func addTool(tools map[string]struct{}, tool string) map[string]struct{} {
	if tools == nil {
		tools = make(map[string]struct{})
	}
	tools[tool] = struct{}{}
	return tools
}

func (lists ruleLists[P]) with(verdict Verdict, rule patternRule[P]) ruleLists[P] {
	switch verdict {
	case Deny:
		lists.deny = append(lists.deny, rule)
	case Allow:
		lists.allow = append(lists.allow, rule)
	case Ask:
		panic("permission: a rule cannot have the verdict ask")
	}
	return lists
}

var errUnbalanced = errors.New("unbalanced parentheses")

func splitRule(text string) (tool, pattern string, hasPattern bool, err error) {
	tool, rest, hasPattern := strings.Cut(text, "(")
	if strings.Contains(tool, ")") {
		return "", "", false, errUnbalanced
	}
	if !validToolName(tool) {
		return "", "", false, fmt.Errorf("tool name %q is not valid; use a lowercase letter, then lowercase letters, digits or _", tool)
	}
	if !hasPattern {
		return tool, "", false, nil
	}
	pattern, closed := strings.CutSuffix(rest, ")")
	if !closed || !balanced(pattern) {
		return "", "", false, errUnbalanced
	}
	if pattern == "" {
		return "", "", false, errors.New("pattern is empty")
	}
	return tool, pattern, true, nil
}

func validToolName(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		character := name[i]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

// balanced rejects patterns such as "rm -rf *)", which come from a typo in
// the rule and would silently fail to match.
func balanced(pattern string) bool {
	depth := 0
	for i := range len(pattern) {
		switch pattern[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// Empty reports whether there are no rules.
func (rules Rules) Empty() bool {
	return len(rules.denyAll) == 0 && len(rules.allowAll) == 0 && len(rules.files) == 0 &&
		rules.commands.empty() && rules.mcpCalls.empty() && rules.webFetches.empty()
}

func (lists ruleLists[P]) empty() bool {
	return len(lists.deny) == 0 && len(lists.allow) == 0
}

// Check decides one tool call. root is the absolute working directory;
// relative paths in the input are relative to it. The returned text is the
// rule that decided, or "" for Ask.
//
// A deny rule always wins over an allow rule. A bare deny rule decides before
// the input is read. Input that is not valid for the tool never gets Allow.
func (rules Rules) Check(root string, tool string, input json.RawMessage) (Verdict, string) {
	if !filepath.IsAbs(root) {
		panic(fmt.Sprintf("permission: root %q is not absolute", root))
	}
	if _, denied := rules.denyAll[tool]; denied {
		return Deny, tool
	}
	_, allowed := rules.allowAll[tool]
	switch kind := kindOf(tool); kind {
	case shellTool:
		return decide(rules.commands, allowed, tool, func() (string, bool) { return decodeCommand(input) })
	case fileTool:
		return decide(rules.files[tool], allowed, tool, func() (target, bool) { return decodeFile(root, input) })
	case mcpTool:
		return decide(rules.mcpCalls, allowed, tool, func() (mcpCall, bool) { return decodeMCPCall(input) })
	case webTool:
		return decide(rules.webFetches, allowed, tool, func() (string, bool) { return decodeHost(input) })
	case customTool:
		if allowed && isObject(input) {
			return Allow, tool
		}
		return Ask, ""
	default:
		panic(fmt.Sprintf("permission: unknown tool kind %d", kind))
	}
}

// decide reads the input only when a rule needs it.
func decide[C any, P matcher[C]](lists ruleLists[P], allowed bool, tool string, decode func() (C, bool)) (Verdict, string) {
	if lists.empty() && !allowed {
		return Ask, ""
	}
	call, valid := decode()
	if !valid {
		return Ask, ""
	}
	for _, rule := range lists.deny {
		if rule.pattern.denies(call) {
			return Deny, rule.text
		}
	}
	if allowed {
		return Allow, tool
	}
	for _, rule := range lists.allow {
		if rule.pattern.allows(call) {
			return Allow, rule.text
		}
	}
	return Ask, ""
}

func isObject(input json.RawMessage) bool {
	trimmed := bytes.TrimLeft(input, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

// mcpPattern holds the server and tool parts of "server/tool". Each part
// uses '*' as its only wildcard. The parts match separately, so a "/" in a
// server or tool name cannot move text from one part to the other.
type mcpPattern struct {
	server, tool string
}

type mcpCall struct {
	server, tool string
}

func parseMCPPattern(pattern string) (mcpPattern, error) {
	server, tool, found := strings.Cut(pattern, "/")
	if !found || server == "" || tool == "" {
		return mcpPattern{}, errors.New("pattern must have the form server/tool, for example github/*")
	}
	return mcpPattern{server: server, tool: tool}, nil
}

func decodeMCPCall(input json.RawMessage) (mcpCall, bool) {
	var arguments struct {
		Server *string `json:"server"`
		Tool   *string `json:"tool"`
	}
	if err := json.Unmarshal(input, &arguments); err != nil || arguments.Server == nil || arguments.Tool == nil {
		return mcpCall{}, false
	}
	return mcpCall{server: *arguments.Server, tool: *arguments.Tool}, true
}

func (pattern mcpPattern) denies(call mcpCall) bool {
	return pattern.matches(call)
}

func (pattern mcpPattern) allows(call mcpCall) bool {
	return pattern.matches(call)
}

func (pattern mcpPattern) matches(call mcpCall) bool {
	return matchWildcard(pattern.server, call.server) && matchWildcard(pattern.tool, call.tool)
}

// matchWildcard reports whether text matches pattern. '*' matches any run
// of bytes, also an empty run; every other byte matches only itself.
func matchWildcard(pattern, text string) bool {
	patternIndex, textIndex := 0, 0
	star, resume := -1, 0
	for textIndex < len(text) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			star, resume = patternIndex, textIndex
			patternIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == text[textIndex]:
			patternIndex++
			textIndex++
		case star >= 0:
			// Let the last star take one more byte, then try again.
			resume++
			patternIndex, textIndex = star+1, resume
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}
