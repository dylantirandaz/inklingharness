package web

import (
	"fmt"
	"mime"
	"strings"
	"unicode/utf8"
)

// textKind tells how the body becomes text for the model.
type textKind uint8

const (
	plainText textKind = iota + 1
	htmlText
)

// charset is a character encoding that web_fetch can decode.
type charset uint8

const (
	utf8Charset charset = iota + 1
	windows1252Charset
)

type textFormat struct {
	kind    textKind
	charset charset
}

// parseContentType reads a Content-Type value. A type that is not text, or a
// charset that cannot be decoded, is an error for the model.
func parseContentType(value string) (textFormat, error) {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil {
		return textFormat{}, fmt.Errorf("the Content-Type %q is not valid", value)
	}
	kind, known := textKindOf(mediaType)
	if !known {
		return textFormat{}, fmt.Errorf("the content type %s is not supported; web_fetch reads only HTML, text, JSON and XML", mediaType)
	}
	label := parameters["charset"]
	set, known := charsetOf(strings.ToLower(strings.TrimSpace(label)))
	if !known {
		return textFormat{}, fmt.Errorf("the charset %q is not supported; web_fetch reads UTF-8, US-ASCII, ISO-8859-1 and windows-1252", label)
	}
	return textFormat{kind: kind, charset: set}, nil
}

func textKindOf(mediaType string) (textKind, bool) {
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		return htmlText, true
	case strings.HasPrefix(mediaType, "text/"),
		mediaType == "application/json" || mediaType == "application/xml",
		// Structured suffixes, for example application/ld+json or
		// application/atom+xml. image/svg+xml is an image, so only the
		// application tree counts.
		strings.HasPrefix(mediaType, "application/") && (strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")):
		return plainText, true
	default:
		return 0, false
	}
}

func charsetOf(label string) (charset, bool) {
	switch label {
	case "", "utf-8", "utf8", "unicode-1-1-utf-8", "us-ascii", "ascii":
		return utf8Charset, true
	// Browsers decode the ISO-8859-1 labels as windows-1252, and servers
	// expect this. The two differ only in the bytes 0x80 to 0x9F.
	case "iso-8859-1", "iso8859-1", "iso_8859-1", "latin1", "latin-1", "l1", "cp819", "ibm819",
		"windows-1252", "cp1252", "x-cp1252":
		return windows1252Charset, true
	default:
		return 0, false
	}
}

// windows1252High holds the characters of the bytes 0x80 to 0x9F. The five
// bytes without a character keep their C1 code point, as in browsers;
// cleanText removes them later.
var windows1252High = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
}

// decode returns body as valid UTF-8. Bytes that are not valid UTF-8 become
// U+FFFD.
func decode(body []byte, set charset) string {
	switch set {
	case utf8Charset:
		text := strings.TrimPrefix(string(body), "\uFEFF")
		return strings.ToValidUTF8(text, "\uFFFD")
	case windows1252Charset:
		var text strings.Builder
		text.Grow(len(body) + len(body)/8)
		for _, value := range body {
			switch {
			case value < 0x80:
				text.WriteByte(value)
			case value < 0xA0:
				text.WriteRune(windows1252High[value-0x80])
			default:
				text.WriteRune(rune(value))
			}
		}
		return text.String()
	default:
		panic(fmt.Sprintf("web: unknown charset %d", set))
	}
}

// isControl reports a character that can control a terminal: C0 controls
// other than tab and line feed, DEL, and C1 controls.
func isControl(character rune) bool {
	return (character < 0x20 && character != '\n' && character != '\t') || (character >= 0x7F && character <= 0x9F)
}

// cleanText removes terminal control characters from valid UTF-8 text.
// "\r\n" and a lone "\r" become "\n", so line breaks stay.
func cleanText(text string) string {
	if !strings.ContainsFunc(text, isControl) {
		return text
	}
	var clean strings.Builder
	clean.Grow(len(text))
	for index, character := range text {
		switch {
		case character == '\r':
			if !strings.HasPrefix(text[index+1:], "\n") {
				clean.WriteByte('\n')
			}
		case isControl(character):
			// Drop the character.
		default:
			clean.WriteRune(character)
		}
	}
	return clean.String()
}

// slice returns at most maxChars characters of text from the 1-based
// character offset, the total number of characters, and the number of
// characters in the part. The part also stays within maxSliceBytes.
func slice(text string, offset, maxChars int) (part string, total, taken int) {
	total = utf8.RuneCountInString(text)
	if offset > total {
		return "", total, 0
	}
	start := 0
	for skipped := 1; skipped < offset; skipped++ {
		_, size := utf8.DecodeRuneInString(text[start:])
		start += size
	}
	end := start
	for taken < maxChars && end < len(text) {
		_, size := utf8.DecodeRuneInString(text[end:])
		if end+size-start > maxSliceBytes {
			break
		}
		end += size
		taken++
	}
	return text[start:end], total, taken
}
