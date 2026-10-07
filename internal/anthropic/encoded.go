package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// EncodedMessage is one completed conversation message in compact Messages
// API wire JSON. It is the stored form of history: requests and session files
// reuse these bytes, so a completed message is encoded once. The bytes never
// change after construction. The zero value is not a message; requests reject
// it.
type EncodedMessage struct {
	role          Role
	wire          []byte
	estimateBytes int
}

// EncodeMessage encodes a typed message once.
func EncodeMessage(message Message) (EncodedMessage, error) {
	if message.Role != RoleUser && message.Role != RoleAssistant {
		return EncodedMessage{}, fmt.Errorf("anthropic: unknown message role %q", message.Role)
	}
	size, err := estimateBytes(message)
	if err != nil {
		return EncodedMessage{}, err
	}
	wire, err := message.MarshalJSON()
	if err != nil {
		return EncodedMessage{}, err
	}
	return EncodedMessage{role: message.Role, wire: wire, estimateBytes: size}, nil
}

// ParseEncodedMessage validates stored wire JSON and computes its metadata in
// one decode. It accepts and rejects exactly what Message.UnmarshalJSON does,
// but it measures strings instead of copying them, so a session load does not
// build a typed copy of the history only to discard it. The result keeps
// wire; the caller must not change it afterward and should pass a clone when
// wire is part of a larger buffer that must not stay in memory.
func ParseEncodedMessage(wire []byte) (EncodedMessage, error) {
	var measured struct {
		Role    Role            `json:"role"`
		Content []measuredBlock `json:"content"`
	}
	if err := json.Unmarshal(wire, &measured); err != nil {
		return EncodedMessage{}, err
	}
	if measured.Role != RoleUser && measured.Role != RoleAssistant {
		return EncodedMessage{}, fmt.Errorf("anthropic: unknown message role %q", measured.Role)
	}
	size := 32
	for _, block := range measured.Content {
		size += int(block)
	}
	return EncodedMessage{role: measured.Role, wire: wire, estimateBytes: size}, nil
}

// measuredBlock is the estimate size of one block, computed with the rules of
// blockDecoder and estimateBytes.
type measuredBlock int

func (b *measuredBlock) UnmarshalJSON(raw []byte) error {
	var fields struct {
		Type      string              `json:"type"`
		Text      measuredString      `json:"text"`
		Thinking  measuredString      `json:"thinking"`
		Signature measuredString      `json:"signature"`
		Data      measuredString      `json:"data"`
		ID        measuredString      `json:"id"`
		Name      measuredString      `json:"name"`
		Input     measuredRaw         `json:"input"`
		ToolUseID measuredString      `json:"tool_use_id"`
		Content   measuredString      `json:"content"`
		IsError   bool                `json:"is_error"`
		Source    measuredImageSource `json:"source"`
	}
	err := json.Unmarshal(raw, &fields)
	if !knownBlock(fields.Type, fields.Source.isBase64) {
		if len(raw) == 0 || raw[0] != '{' {
			return errors.New("content block is not a JSON object")
		}
		*b = measuredBlock(len(raw))
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s block: %w", fields.Type, err)
	}
	// blockDecoder decodes these fields as strings for every known kind.
	for _, field := range [...]measuredString{fields.Text, fields.Thinking, fields.Signature, fields.Data, fields.ID, fields.Name, fields.ToolUseID} {
		if field.notString {
			return fmt.Errorf("%s block: a string field has another JSON type", fields.Type)
		}
	}
	switch fields.Type {
	case "text":
		*b = measuredBlock(fields.Text.length)
	case "thinking":
		*b = measuredBlock(fields.Thinking.length + fields.Signature.length)
	case "redacted_thinking":
		*b = measuredBlock(fields.Data.length)
	case "tool_use":
		*b = measuredBlock(fields.ID.length + fields.Name.length + int(fields.Input))
	case "tool_result":
		if fields.Content.notString {
			return errors.New("tool_result block: content is not a string")
		}
		*b = measuredBlock(fields.ToolUseID.length + fields.Content.length)
	case "image":
		if fields.Source.err != nil {
			return fmt.Errorf("image block: %w", fields.Source.err)
		}
		*b = imageEstimateBytes
	}
	return nil
}

// measuredImageSource checks a "source" field with the rules of imageSource,
// but it does not copy the image data.
type measuredImageSource struct {
	isBase64 bool
	err      error
}

func (s *measuredImageSource) UnmarshalJSON(raw []byte) error {
	*s = measuredImageSource{}
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var fields struct {
		Type      string            `json:"type"`
		MediaType string            `json:"media_type"`
		Data      measuredImageData `json:"data"`
	}
	// An absent or null "data" leaves a Go string empty, so start from the
	// result for empty data.
	fields.Data.err = checkImageData("")
	err := json.Unmarshal(raw, &fields)
	if err == nil && fields.Data.notString {
		err = errors.New("image data is not a string")
	}
	if err == nil {
		err = checkImageMediaType(fields.MediaType)
	}
	if err == nil {
		err = fields.Data.err
	}
	*s = measuredImageSource{isBase64: fields.Type == "base64", err: err}
	return nil
}

// measuredImageData checks the base64 data in place. A Go string field keeps
// the last of repeated keys, but a type mismatch in any of them is an error,
// so notString stays set and err follows the last string.
type measuredImageData struct {
	notString bool
	err       error
}

func (d *measuredImageData) UnmarshalJSON(raw []byte) error {
	switch {
	case string(raw) == "null":
		// null leaves a Go string field unchanged.
	case len(raw) > 0 && raw[0] == '"':
		if bytes.IndexByte(raw, '\\') < 0 {
			d.err = checkImageData(raw[1 : len(raw)-1])
			return nil
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		d.err = checkImageData(text)
	default:
		d.notString = true
	}
	return nil
}

// measuredString is the decoded byte length of a JSON string. null decodes as
// an empty string, as it does for a Go string field.
type measuredString struct {
	length    int
	notString bool
}

func (s *measuredString) UnmarshalJSON(raw []byte) error {
	switch {
	case string(raw) == "null":
	case len(raw) > 0 && raw[0] == '"':
		s.length = unquotedLength(raw)
	default:
		s.notString = true
	}
	return nil
}

// measuredRaw is the length that a json.RawMessage field would hold.
type measuredRaw int

func (r *measuredRaw) UnmarshalJSON(raw []byte) error {
	*r = measuredRaw(len(raw))
	return nil
}

// unquotedLength returns the byte length that encoding/json produces when it
// decodes the valid JSON string literal quoted. It follows the same rules:
// a valid surrogate pair is one rune, and an invalid surrogate or an invalid
// UTF-8 byte becomes U+FFFD.
func unquotedLength(quoted []byte) int {
	text := quoted[1 : len(quoted)-1]
	length := 0
	for index := 0; index < len(text); {
		switch character := text[index]; {
		case character == '\\' && text[index+1] == 'u':
			value := hexRune(text[index+2 : index+6])
			index += 6
			if utf16.IsSurrogate(value) {
				if index+6 <= len(text) && text[index] == '\\' && text[index+1] == 'u' {
					if pair := utf16.DecodeRune(value, hexRune(text[index+2:index+6])); pair != unicode.ReplacementChar {
						length += utf8.RuneLen(pair)
						index += 6
						continue
					}
				}
				value = unicode.ReplacementChar
			}
			length += utf8.RuneLen(value)
		case character == '\\':
			length++
			index += 2
		case character < utf8.RuneSelf:
			length++
			index++
		default:
			value, size := utf8.DecodeRune(text[index:])
			if value == utf8.RuneError && size == 1 {
				length += utf8.RuneLen(utf8.RuneError)
			} else {
				length += size
			}
			index += size
		}
	}
	return length
}

// hexRune decodes four hexadecimal digits that the JSON scanner already
// validated.
func hexRune(digits []byte) rune {
	var value rune
	for _, digit := range digits {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= rune(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= rune(digit - 'a' + 10)
		default:
			value |= rune(digit - 'A' + 10)
		}
	}
	return value
}

// Role is the author of the message.
func (m EncodedMessage) Role() Role { return m.role }

// Wire returns the stored JSON. The slice is shared and read-only.
func (m EncodedMessage) Wire() []byte { return m.wire }

// EstimateBytes is 32 bytes of message overhead plus the content size of each
// block. Context estimates divide the sum by three.
func (m EncodedMessage) EstimateBytes() int { return m.estimateBytes }

// Decode returns a new typed copy for code that needs block contents.
func (m EncodedMessage) Decode() (Message, error) {
	if m.wire == nil {
		return Message{}, errors.New("anthropic: decode empty encoded message")
	}
	var message Message
	if err := message.UnmarshalJSON(m.wire); err != nil {
		return Message{}, err
	}
	return message, nil
}

// Equal reports whether both values hold the same wire bytes.
func (m EncodedMessage) Equal(other EncodedMessage) bool {
	return m.role == other.role && bytes.Equal(m.wire, other.wire)
}

// MarshalJSON returns the stored wire JSON.
func (m EncodedMessage) MarshalJSON() ([]byte, error) {
	if m.wire == nil {
		return nil, errors.New("anthropic: encode empty encoded message")
	}
	return m.wire, nil
}

// UnmarshalJSON copies and validates one message.
func (m *EncodedMessage) UnmarshalJSON(data []byte) error {
	parsed, err := ParseEncodedMessage(bytes.Clone(data))
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
