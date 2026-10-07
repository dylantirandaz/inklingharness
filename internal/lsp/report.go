package lsp

import (
	"cmp"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// maxReportLines caps the diagnostics in one report, so a broken file does
	// not fill the context window.
	maxReportLines = 20
	// maxMessageRunes caps the message of one diagnostic.
	maxMessageRunes = 300
)

// severity is the DiagnosticSeverity of the protocol.
type severity int

const (
	severityError       severity = 1
	severityWarning     severity = 2
	severityInformation severity = 3
	severityHint        severity = 4
)

type diagnostic struct {
	Range struct {
		Start position `json:"start"`
	} `json:"range"`
	Severity severity `json:"severity"`
	Message  string   `json:"message"`
}

// position is zero-based. The character counts UTF-16 code units, which is
// the character column for ASCII text.
type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// report gives the errors and then the warnings for one file, one per line.
// It is "" when there are none.
func report(root, path string, diagnostics []diagnostic) string {
	shown := make([]diagnostic, 0, len(diagnostics))
	for _, item := range diagnostics {
		switch item.Severity {
		case severityError, severityWarning:
			shown = append(shown, item)
		case severityInformation, severityHint:
		default:
			// The server sent no severity or an unknown one. The report keeps
			// only problems that the server calls errors or warnings.
		}
	}
	if len(shown) == 0 {
		return ""
	}
	slices.SortStableFunc(shown, func(a, b diagnostic) int {
		return cmp.Or(
			cmp.Compare(a.Severity, b.Severity),
			cmp.Compare(a.Range.Start.Line, b.Range.Start.Line),
			cmp.Compare(a.Range.Start.Character, b.Range.Start.Character),
		)
	})
	name := displayPath(root, path)
	var text strings.Builder
	for index, item := range shown[:min(len(shown), maxReportLines)] {
		if index > 0 {
			text.WriteByte('\n')
		}
		fmt.Fprintf(&text, "%s:%d:%d: %s: %s", name, item.Range.Start.Line+1, item.Range.Start.Character+1, item.Severity.label(), shortMessage(item.Message))
	}
	if hidden := len(shown) - maxReportLines; hidden > 0 {
		fmt.Fprintf(&text, "\n(%d more)", hidden)
	}
	return text.String()
}

// label is defined only for the severities that a report shows.
func (s severity) label() string {
	switch s {
	case severityError:
		return "error"
	case severityWarning:
		return "warning"
	case severityInformation, severityHint:
		panic(fmt.Sprintf("lsp: severity %d is not in reports", s))
	default:
		panic(fmt.Sprintf("lsp: unknown severity %d", s))
	}
}

// displayPath is relative to the root when the file is inside it.
func displayPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return path
	}
	return relative
}

// shortMessage puts a message on one line and cuts it to maxMessageRunes.
func shortMessage(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if utf8.RuneCountInString(message) <= maxMessageRunes {
		return message
	}
	cut := 0
	for range maxMessageRunes - 1 {
		_, size := utf8.DecodeRuneInString(message[cut:])
		cut += size
	}
	return message[:cut] + "…"
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// uriPath gives the clean path of a file URI. Servers can encode a URI in a
// different way than the client did, so the client compares paths.
func uriPath(uri string) (string, bool) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" || (parsed.Host != "" && parsed.Host != "localhost") || !filepath.IsAbs(parsed.Path) {
		return "", false
	}
	return filepath.Clean(parsed.Path), true
}

// languageID gives the language identifier of the protocol for an extension.
func languageID(extension string) string {
	switch extension {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".py":
		return "python"
	case ".c":
		return "c"
	case ".h", ".cc", ".cpp", ".hpp":
		// Editors give .h to C++, because C++ code reads C headers too.
		return "cpp"
	default:
		// Most identifiers in the protocol are the extension without the
		// dot. This is the best guess for a configured server.
		return strings.TrimPrefix(extension, ".")
	}
}
