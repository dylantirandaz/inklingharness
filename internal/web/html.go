package web

import (
	"html"
	"net/url"
	"strings"
	"unicode"
)

// htmlToText converts an HTML page to Markdown-like text and returns the
// title and the text. It is a small tokenizer, not a full HTML parser: it
// does not repair bad nesting, but it never fails, also on a page that was
// cut in the middle of a tag. Relative links resolve against base.
func htmlToText(page string, base *url.URL) (title, text string) {
	converter := converter{base: base}
	converter.convert(page)
	return converter.title, converter.out.String()
}

type converter struct {
	base *url.URL
	out  strings.Builder
	// The converter writes line breaks and spaces only before the next
	// visible text, so that whitespace never collects at the end or at the
	// start of a line.
	pendingBreaks int
	pendingSpace  bool
	// markerEnd is the output length after the last list or heading marker.
	markerEnd int
	// skipping names the element whose content is dropped, or is "".
	// skipDepth counts the open elements with that name.
	skipping  string
	skipDepth int
	listDepth int
	codeDepth int
	// Text in pre elements collects in pre and goes out as one fenced block.
	preDepth int
	pre      strings.Builder
	// link is the absolute URL of the open link, or "". linkStart is the
	// output length when the link opened.
	link      string
	linkStart int
	title     string
}

type startTag struct {
	name        string
	href        string
	selfClosing bool
}

func (c *converter) convert(page string) {
	for page != "" {
		open := strings.IndexByte(page, '<')
		if open < 0 {
			c.text(page)
			break
		}
		c.text(page[:open])
		page = page[open:]
		switch {
		case strings.HasPrefix(page, "<!--"):
			end := strings.Index(page[len("<!--"):], "-->")
			if end < 0 {
				page = ""
				break
			}
			page = page[len("<!--")+end+len("-->"):]
		case strings.HasPrefix(page, "<!"), strings.HasPrefix(page, "<?"):
			page = after(page, '>')
		case len(page) > 2 && page[1] == '/' && isASCIILetter(page[2]):
			nameEnd := 2
			for nameEnd < len(page) && !isTagNameEnd(page[nameEnd]) {
				nameEnd++
			}
			c.endTag(strings.ToLower(page[2:nameEnd]))
			page = after(page[nameEnd:], '>')
		case len(page) > 1 && isASCIILetter(page[1]):
			tag, length, complete := parseStartTag(page)
			if !complete {
				page = ""
				break
			}
			page = c.element(tag, page[length:])
		default:
			c.text("<")
			page = page[1:]
		}
	}
	c.closeLink()
	if c.preDepth > 0 {
		c.preDepth = 0
		c.writePre()
	}
}

// element handles one start tag and returns the rest of the page. Script,
// style and title hold raw text, which the tokenizer must not read as
// markup.
func (c *converter) element(tag startTag, rest string) string {
	switch tag.name {
	case "script", "style":
		_, rest = splitRawText(rest, tag.name)
	case "title":
		var content string
		content, rest = splitRawText(rest, tag.name)
		if c.title == "" && (c.skipping == "" || c.skipping == "head") {
			c.title = strings.Join(strings.Fields(html.UnescapeString(content)), " ")
		}
	default:
		c.startTag(tag)
	}
	return rest
}

func (c *converter) startTag(tag startTag) {
	if c.skipping == "head" && !headElement(tag.name) {
		// A body element closes a head without an end tag.
		c.skipping, c.skipDepth = "", 0
	}
	if c.skipping != "" {
		if tag.name == c.skipping && !tag.selfClosing {
			c.skipDepth++
		}
		return
	}
	if c.preDepth > 0 {
		switch tag.name {
		case "pre":
			c.preDepth++
		case "br":
			c.pre.WriteByte('\n')
		}
		return
	}
	switch tag.name {
	case "head", "noscript", "svg", "template":
		if !tag.selfClosing {
			c.skipping, c.skipDepth = tag.name, 1
		}
	case "h1", "h2", "h3", "h4", "h5", "h6":
		c.openBlock(2)
		c.marker(strings.Repeat("#", int(tag.name[1]-'0')) + " ")
	case "p":
		c.openBlock(c.paragraphBreaks())
	case "br", "hr", "div", "tr", "table", "section", "article", "header", "footer", "nav", "main",
		"aside", "blockquote", "figure", "dt", "dd":
		c.openBlock(1)
	case "ul", "ol":
		c.listDepth++
		c.openBlock(1)
	case "li":
		c.openBlock(1)
		c.marker(strings.Repeat("  ", max(c.listDepth-1, 0)) + "- ")
	case "td", "th":
		c.space()
	case "a":
		// Links do not nest, so a new link closes the open one.
		c.closeLink()
		c.openLink(tag.href)
	case "pre":
		c.preDepth = 1
	case "code":
		c.write("`")
		c.codeDepth++
	default:
		// Other elements, for example span or b, do not change the layout.
	}
}

func (c *converter) endTag(name string) {
	if c.skipping != "" {
		if name == c.skipping {
			c.skipDepth--
			if c.skipDepth == 0 {
				c.skipping = ""
			}
		}
		return
	}
	if c.preDepth > 0 {
		if name == "pre" {
			c.preDepth--
			if c.preDepth == 0 {
				c.writePre()
			}
		}
		return
	}
	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		c.breakLine(2)
	case "p":
		c.breakLine(c.paragraphBreaks())
	case "div", "tr", "table", "section", "article", "header", "footer", "nav", "main",
		"aside", "blockquote", "figure", "dt", "dd", "li":
		c.breakLine(1)
	case "ul", "ol":
		c.listDepth = max(c.listDepth-1, 0)
		c.breakLine(1)
	case "a":
		c.closeLink()
	case "code":
		if c.codeDepth > 0 {
			c.codeDepth--
			c.out.WriteString("`")
		}
	default:
		// Other end tags do not change the layout.
	}
}

// paragraphBreaks gives an empty line around a paragraph, but not in a list,
// where paragraphs in items would split the list.
func (c *converter) paragraphBreaks() int {
	if c.listDepth > 0 {
		return 1
	}
	return 2
}

// headElement reports the elements that can be in a head element.
func headElement(name string) bool {
	switch name {
	case "title", "meta", "link", "style", "script", "noscript", "base", "template":
		return true
	default:
		return false
	}
}

func (c *converter) text(raw string) {
	if c.skipping == "head" && strings.TrimLeft(raw, " \t\r\n\f") != "" {
		// Visible text closes a head without an end tag.
		c.skipping, c.skipDepth = "", 0
	}
	if c.skipping != "" || raw == "" {
		return
	}
	decoded := html.UnescapeString(raw)
	if c.preDepth > 0 {
		c.pre.WriteString(decoded)
		return
	}
	for _, character := range decoded {
		if unicode.IsSpace(character) {
			c.space()
			continue
		}
		c.flush()
		c.out.WriteRune(character)
	}
}

// space asks for one space before the next text, unless the output is at
// the start of a line or ends with a space.
func (c *converter) space() {
	written := c.out.String()
	if written == "" || c.pendingBreaks > 0 {
		return
	}
	if last := written[len(written)-1]; last != ' ' && last != '\n' {
		c.pendingSpace = true
	}
}

// breakLine asks for count line breaks before the next text. A count of 2
// gives an empty line.
func (c *converter) breakLine(count int) {
	c.pendingBreaks = max(c.pendingBreaks, count)
	c.pendingSpace = false
}

// openBlock is breakLine for a start tag. Directly after a list or heading
// marker it does nothing, so "<li><p>text" gives "- text".
func (c *converter) openBlock(count int) {
	if c.out.Len() == c.markerEnd {
		return
	}
	c.breakLine(count)
}

func (c *converter) flush() {
	switch {
	case c.out.Len() == 0:
	case c.pendingBreaks > 0:
		c.out.WriteString("\n\n"[:min(c.pendingBreaks, 2)])
	case c.pendingSpace:
		c.out.WriteByte(' ')
	}
	c.pendingBreaks, c.pendingSpace = 0, false
}

func (c *converter) write(text string) {
	c.flush()
	c.out.WriteString(text)
}

func (c *converter) marker(text string) {
	c.write(text)
	c.markerEnd = c.out.Len()
}

func (c *converter) openLink(href string) {
	href = strings.TrimSpace(href)
	// A link to a part of the same page adds only noise.
	if href == "" || strings.HasPrefix(href, "#") {
		return
	}
	reference, err := url.Parse(href)
	if err != nil {
		return
	}
	target := c.base.ResolveReference(reference)
	if target.Scheme != "http" && target.Scheme != "https" && target.Scheme != "mailto" {
		return
	}
	c.link = target.String()
	c.linkStart = c.out.Len()
}

func (c *converter) closeLink() {
	if c.link == "" {
		return
	}
	link := c.link
	c.link = ""
	switch strings.TrimSpace(c.out.String()[c.linkStart:]) {
	case "":
		c.write(link)
	case link:
		// The text is the URL already.
	default:
		c.out.WriteString(" (" + link + ")")
	}
}

func (c *converter) writePre() {
	// HTML ignores the line break after <pre>; empty lines at the edges of a
	// code block carry no meaning.
	content := strings.TrimRight(strings.TrimLeft(c.pre.String(), "\r\n"), " \t\r\n")
	c.pre.Reset()
	if content == "" {
		return
	}
	fence := codeFence(content)
	c.breakLine(1)
	c.write(fence + "\n" + content + "\n" + fence)
	c.breakLine(1)
}

// codeFence returns a fence longer than every run of backticks in content,
// so the content cannot close the block early.
func codeFence(content string) string {
	longest, run := 0, 0
	for index := range len(content) {
		if content[index] == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// parseStartTag reads the start tag at the start of page, which begins with
// '<' and a letter. It keeps only the href attribute. complete is false when
// the page ends inside the tag.
func parseStartTag(page string) (tag startTag, length int, complete bool) {
	index := 1
	for index < len(page) && !isTagNameEnd(page[index]) {
		index++
	}
	tag.name = strings.ToLower(page[1:index])
	for index < len(page) {
		switch character := page[index]; {
		case character == '>':
			return tag, index + 1, true
		case character == '/':
			index++
			if index < len(page) && page[index] == '>' {
				tag.selfClosing = true
				return tag, index + 1, true
			}
		case isHTMLSpace(character):
			index++
		default:
			nameStart := index
			for index < len(page) && !isTagNameEnd(page[index]) && page[index] != '=' {
				index++
			}
			name := page[nameStart:index]
			index = skipSpace(page, index)
			value := ""
			if index < len(page) && page[index] == '=' {
				value, index = attributeValue(page, skipSpace(page, index+1))
			}
			if tag.href == "" && strings.EqualFold(name, "href") {
				tag.href = html.UnescapeString(value)
			}
		}
	}
	return startTag{}, 0, false
}

// attributeValue reads a quoted or unquoted value at index and returns it
// with the index after it.
func attributeValue(page string, index int) (string, int) {
	if index >= len(page) {
		return "", index
	}
	if quote := page[index]; quote == '"' || quote == '\'' {
		end := strings.IndexByte(page[index+1:], quote)
		if end < 0 {
			return "", len(page)
		}
		return page[index+1 : index+1+end], index + end + 2
	}
	start := index
	for index < len(page) && !isHTMLSpace(page[index]) && page[index] != '>' {
		index++
	}
	return page[start:index], index
}

// splitRawText splits the text after a raw text start tag at the end tag
// with the given name. Without an end tag, all text is content.
func splitRawText(page, name string) (content, rest string) {
	for from := 0; ; {
		open := strings.Index(page[from:], "</")
		if open < 0 {
			return page, ""
		}
		start := from + open
		nameEnd := start + 2 + len(name)
		if nameEnd <= len(page) && strings.EqualFold(page[start+2:nameEnd], name) &&
			(nameEnd == len(page) || isTagNameEnd(page[nameEnd])) {
			return page[:start], after(page[nameEnd:], '>')
		}
		from = start + 2
	}
}

// after returns the text after the first separator, or "" when there is no
// separator.
func after(text string, separator byte) string {
	index := strings.IndexByte(text, separator)
	if index < 0 {
		return ""
	}
	return text[index+1:]
}

func skipSpace(page string, index int) int {
	for index < len(page) && isHTMLSpace(page[index]) {
		index++
	}
	return index
}

func isHTMLSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r' || character == '\f'
}

func isTagNameEnd(character byte) bool {
	return isHTMLSpace(character) || character == '/' || character == '>'
}

func isASCIILetter(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
}
