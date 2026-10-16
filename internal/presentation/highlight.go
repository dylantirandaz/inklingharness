package presentation

import "strings"

// Code fences are colored one line at a time, in one pass over the bytes, so
// streaming cost stays linear in the reply and no line is ever re-scanned
// after it is committed. Only the state that crosses a line break (an open
// block comment or multi-line string) is kept between lines.

type family uint8

const (
	familyNone family = iota
	familyC           // Go, JavaScript, TypeScript, Rust, C, Java, ...
	familyHash        // Python, shell, YAML, TOML, Ruby
	familyJSON
	familySQL
	familyDiff
)

type language struct {
	family   family
	keywords map[string]struct{}
	// backtick strings span lines (Go raw strings, JavaScript templates).
	backtick bool
	// triple quotes span lines (Python).
	triple bool
	// variables color $NAME (shell).
	variables bool
	// keys color a leading NAME before ':' or '=' (YAML, TOML).
	keys bool
	// fold matches keywords case-insensitively (SQL).
	fold bool
}

// codeState is what one fence line leaves open for the next.
type codeState struct {
	lang    *language
	comment bool   // inside /* */
	str     string // open multi-line string delimiter
}

func words(list string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, word := range strings.Fields(list) {
		set[word] = struct{}{}
	}
	return set
}

var (
	goLang = &language{family: familyC, backtick: true, keywords: words(`break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var
		nil true false iota any error string int int64 int32 uint uint8 uint64 byte rune bool float64 float32 make new len cap append copy delete panic recover`)}
	jsLang = &language{family: familyC, backtick: true, keywords: words(`async await break case catch class const continue debugger default delete do else export extends finally for from function if import in instanceof let new of return static super switch this throw try typeof var void while with yield
		null undefined true false type interface enum implements readonly private public protected as satisfies keyof declare namespace abstract string number boolean unknown never any`)}
	cLang = &language{family: familyC, keywords: words(`auto break case char const continue default do double else enum extern float for goto if int long register return short signed sizeof static struct switch typedef union unsigned void volatile while
		bool true false nullptr NULL class public private protected virtual override template typename namespace using new delete this throw try catch final import package extends implements interface abstract
		fn let mut impl trait pub use mod match loop where self Self crate move ref dyn async await unsafe Some None Ok Err
		val var fun when object data sealed func guard defer struct let`)}
	pyLang   = &language{family: familyHash, triple: true, keywords: words(`and as assert async await break class continue def del elif else except finally for from global if import in is lambda match case nonlocal not or pass raise return try while with yield None True False self cls`)}
	shLang   = &language{family: familyHash, variables: true, keywords: words(`if then else elif fi for while until do done case esac in function return export local readonly unset set echo cd exit source alias sudo`)}
	rbLang   = &language{family: familyHash, keywords: words(`begin class def do else elsif end ensure false if module next nil rescue return self super then true unless until when while yield require attr_reader attr_accessor`)}
	cfgLang  = &language{family: familyHash, keys: true, keywords: words(`true false null yes no on off`)}
	jsonLang = &language{family: familyJSON, keywords: words(`true false null`)}
	sqlLang  = &language{family: familySQL, fold: true, keywords: words(`select from where and or not insert into values update set delete create table index drop alter add join left right inner outer on group by order having limit offset as distinct union all null is in like between case when then else end primary key references default with returning count sum avg min max`)}
	diffLang = &language{family: familyDiff}
)

var languages = map[string]*language{
	"go": goLang, "golang": goLang,
	"js": jsLang, "javascript": jsLang, "jsx": jsLang, "mjs": jsLang, "cjs": jsLang,
	"ts": jsLang, "typescript": jsLang, "tsx": jsLang,
	"c": cLang, "h": cLang, "cpp": cLang, "c++": cLang, "cc": cLang, "hpp": cLang, "java": cLang, "kotlin": cLang, "kt": cLang,
	"rust": cLang, "rs": cLang, "swift": cLang, "cs": cLang, "csharp": cLang, "zig": cLang, "scala": cLang, "dart": cLang,
	"python": pyLang, "py": pyLang, "python3": pyLang,
	"sh": shLang, "bash": shLang, "zsh": shLang, "shell": shLang, "console": shLang, "fish": shLang, "dockerfile": shLang, "makefile": shLang, "make": shLang,
	"ruby": rbLang, "rb": rbLang,
	"yaml": cfgLang, "yml": cfgLang, "toml": cfgLang, "ini": cfgLang, "conf": cfgLang, "env": cfgLang,
	"json": jsonLang, "jsonc": jsonLang, "json5": jsonLang, "jsonl": jsonLang,
	"sql": sqlLang, "sqlite": sqlLang, "postgres": sqlLang, "psql": sqlLang,
	"diff": diffLang, "patch": diffLang, "udiff": diffLang,
}

// languageFor reads a fence info string such as "go", "ts title=x" or
// "{.python}". Unknown languages stay plain rather than guessed.
func languageFor(info string) *language {
	info = strings.TrimSpace(info)
	if end := strings.IndexAny(info, " \t,{}"); end == 0 {
		info = strings.Trim(info, "{}. \t")
	}
	if end := strings.IndexAny(info, " \t,{"); end >= 0 {
		info = info[:end]
	}
	return languages[strings.ToLower(strings.TrimPrefix(info, "."))]
}

// highlight colors one fence line and advances state. Text is already Safe.
func highlight(text string, state *codeState, theme Theme) string {
	lang := state.lang
	if lang == nil || !theme.Colored() || text == "" {
		return text
	}
	if lang.family == familyDiff {
		return highlightDiff(text, theme)
	}
	var out strings.Builder
	out.Grow(len(text) + len(text)/2)
	index := 0
	// Finish what the previous line left open.
	if state.comment {
		end := strings.Index(text, "*/")
		if end < 0 {
			return theme.Graphite(text)
		}
		out.WriteString(theme.Graphite(text[:end+2]))
		index = end + 2
		state.comment = false
	} else if state.str != "" {
		end := closing(text, 0, state.str)
		if end < 0 {
			return theme.Green(text)
		}
		out.WriteString(theme.Green(text[:end]))
		index = end
		state.str = ""
	}
	keyDone := !lang.keys
	for index < len(text) {
		c := text[index]
		switch {
		case lang.family == familyC && strings.HasPrefix(text[index:], "//"),
			lang.family == familySQL && strings.HasPrefix(text[index:], "--"),
			lang.family == familyHash && c == '#' && (index == 0 || text[index-1] == ' ' || text[index-1] == '\t'):
			out.WriteString(theme.Graphite(text[index:]))
			return out.String()
		case lang.family == familyC && strings.HasPrefix(text[index:], "/*"):
			end := strings.Index(text[index+2:], "*/")
			if end < 0 {
				state.comment = true
				out.WriteString(theme.Graphite(text[index:]))
				return out.String()
			}
			out.WriteString(theme.Graphite(text[index : index+2+end+2]))
			index += 2 + end + 2
		case c == '"' || c == '\'' || c == '`':
			delimiter := text[index : index+1]
			if lang.triple && (strings.HasPrefix(text[index:], `"""`) || strings.HasPrefix(text[index:], `'''`)) {
				delimiter = text[index : index+3]
			}
			if c == '`' && !lang.backtick && lang.family != familySQL {
				out.WriteByte(c)
				index++
				continue
			}
			if c == '\'' && lang.family == familyC && lang != jsLang && index > 0 && wordByte(text[index-1]) {
				// Rust lifetimes and C++ digit separators are not strings.
				out.WriteByte(c)
				index++
				continue
			}
			end := closing(text, index+len(delimiter), delimiter)
			if end < 0 {
				if len(delimiter) == 3 || c == '`' {
					state.str = delimiter
				}
				out.WriteString(theme.Green(text[index:]))
				return out.String()
			}
			literal := text[index:end]
			if lang.family == familyJSON && nextNonSpace(text, end) == ':' {
				out.WriteString(theme.Blue(literal))
			} else {
				out.WriteString(theme.Green(literal))
			}
			index = end
		case c == '$' && lang.variables && index+1 < len(text) && (identStart(text[index+1]) || text[index+1] == '{'):
			end := index + 1
			if text[end] == '{' {
				if close := strings.IndexByte(text[end:], '}'); close >= 0 {
					end += close + 1
				} else {
					end = len(text)
				}
			} else {
				for end < len(text) && identByte(text[end]) {
					end++
				}
			}
			out.WriteString(theme.Amber(text[index:end]))
			index = end
		case c >= '0' && c <= '9' && (index == 0 || !identByte(text[index-1])):
			end := index + 1
			for end < len(text) && (identByte(text[end]) || text[end] == '.' && end+1 < len(text) && text[end+1] >= '0' && text[end+1] <= '9') {
				end++
			}
			out.WriteString(theme.Amber(text[index:end]))
			index = end
		case identStart(c):
			end := index + 1
			for end < len(text) && (identByte(text[end]) || text[end] == '-' && lang.keys && !keyDone) {
				end++
			}
			word := text[index:end]
			if !keyDone {
				keyDone = true
				if next := nextNonSpace(text, end); next == ':' || next == '=' {
					out.WriteString(theme.Blue(word))
					index = end
					continue
				}
			}
			lookup := word
			if lang.fold {
				lookup = strings.ToLower(word)
			}
			if _, ok := lang.keywords[lookup]; ok {
				out.WriteString(theme.Blue(word))
			} else if next := nextByte(text, end); next == '(' && lang.family != familySQL {
				out.WriteString(theme.Accent(word))
			} else {
				out.WriteString(word)
			}
			index = end
		default:
			if c != ' ' && c != '\t' && c != '-' {
				keyDone = true
			}
			out.WriteByte(c)
			index++
		}
	}
	return out.String()
}

func highlightDiff(text string, theme Theme) string {
	switch {
	case strings.HasPrefix(text, "+++"), strings.HasPrefix(text, "---"), strings.HasPrefix(text, "diff "), strings.HasPrefix(text, "index "):
		return theme.Bold(theme.Graphite(text))
	case strings.HasPrefix(text, "@@"):
		return theme.Blue(text)
	case text[0] == '+':
		return theme.Added(text)
	case text[0] == '-':
		return theme.Removed(text)
	}
	return text
}

// closing returns the index just after the delimiter that closes a string
// opened before start, honoring backslash escapes, or -1 when it stays open.
func closing(text string, start int, delimiter string) int {
	for index := start; index < len(text); index++ {
		if text[index] == '\\' && delimiter != "`" {
			index++
			continue
		}
		if strings.HasPrefix(text[index:], delimiter) {
			return index + len(delimiter)
		}
	}
	return -1
}

func identStart(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_'
}

func identByte(b byte) bool { return identStart(b) || b >= '0' && b <= '9' }

func nextByte(text string, index int) byte {
	if index < len(text) {
		return text[index]
	}
	return 0
}

func nextNonSpace(text string, index int) byte {
	for index < len(text) && (text[index] == ' ' || text[index] == '\t') {
		index++
	}
	return nextByte(text, index)
}
