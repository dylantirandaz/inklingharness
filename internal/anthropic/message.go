package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Role is the author of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of the conversation.
type Message struct {
	Role    Role
	Content []ContentBlock
}

// ContentBlock is one block inside a message. The set of implementations is
// closed to this package: TextBlock, ThinkingBlock, RedactedThinkingBlock,
// ToolUseBlock, ToolResultBlock, ImageBlock, and OpaqueBlock.
type ContentBlock interface {
	contentBlock()
}

// TextBlock holds text from the user or from the model.
type TextBlock struct {
	Text string
}

// ThinkingBlock holds model reasoning. The server verifies Signature when the
// block goes back in a later request, so both fields must stay unchanged.
type ThinkingBlock struct {
	Thinking  string
	Signature string
}

// RedactedThinkingBlock holds encrypted reasoning that the server hides from
// the client. Data must go back unchanged.
type RedactedThinkingBlock struct {
	Data string
}

// ToolUseBlock is a request from the model to run one tool.
type ToolUseBlock struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResultBlock carries the output of one tool run back to the model.
type ToolResultBlock struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// ImageBlock is one image from the user, sent inline. MediaType is one of
// image/png, image/jpeg, image/gif, or image/webp. Data is the standard padded
// base64 encoding of the image bytes. An image with another source kind, such
// as a URL, stays an OpaqueBlock.
type ImageBlock struct {
	MediaType string
	Data      string
}

// ImageEstimateBytes is the estimate size of one image. The API scales a large
// image down, so one image costs at most about 1,600 tokens. The base64 length
// would overstate the cost many times and start compaction too early.
const ImageEstimateBytes = 1600 * 3

// OpaqueBlock holds a block kind that this client does not know. The raw JSON
// goes back to the server unchanged, so a new server block kind does not break
// the conversation.
type OpaqueBlock struct {
	Raw json.RawMessage
}

func (TextBlock) contentBlock()             {}
func (ThinkingBlock) contentBlock()         {}
func (RedactedThinkingBlock) contentBlock() {}
func (ToolUseBlock) contentBlock()          {}
func (ToolResultBlock) contentBlock()       {}
func (ImageBlock) contentBlock()            {}
func (OpaqueBlock) contentBlock()           {}

// MarshalJSON encodes the message in compact Messages API wire format.
func (m Message) MarshalJSON() ([]byte, error) {
	size, _, err := estimateBytes(m)
	if err != nil {
		return nil, err
	}
	encoder := messageEncoder{}
	// Block keys and escapes add bytes beyond the content estimate.
	encoder.buffer.Grow(size + 48*len(m.Content))
	encoder.json = json.NewEncoder(&encoder.buffer)
	encoder.buffer.WriteString(`{"role":`)
	if err := encoder.value(m.Role); err != nil {
		return nil, err
	}
	encoder.buffer.WriteString(`,"content":[`)
	for index, block := range m.Content {
		if index > 0 {
			encoder.buffer.WriteByte(',')
		}
		if err := encoder.block(block); err != nil {
			return nil, err
		}
	}
	encoder.buffer.WriteString(`]}`)
	return encoder.buffer.Bytes(), nil
}

// messageEncoder writes every value into one buffer. json.Encoder adds a
// newline after each value; value removes it so the result stays compact and
// matches json.Marshal byte for byte.
type messageEncoder struct {
	buffer bytes.Buffer
	json   *json.Encoder
}

func (e *messageEncoder) value(value any) error {
	if err := e.json.Encode(value); err != nil {
		return err
	}
	e.buffer.Truncate(e.buffer.Len() - 1)
	return nil
}

func (e *messageEncoder) block(block ContentBlock) error {
	switch typed := block.(type) {
	case TextBlock:
		return e.value(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: typed.Text})
	case ThinkingBlock:
		return e.value(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{Type: "thinking", Thinking: typed.Thinking, Signature: typed.Signature})
	case RedactedThinkingBlock:
		return e.value(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{Type: "redacted_thinking", Data: typed.Data})
	case ToolUseBlock:
		return e.value(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: "tool_use", ID: typed.ID, Name: typed.Name, Input: typed.Input})
	case ToolResultBlock:
		return e.value(struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
			IsError   bool   `json:"is_error,omitempty"`
		}{Type: "tool_result", ToolUseID: typed.ToolUseID, Content: typed.Content, IsError: typed.IsError})
	case ImageBlock:
		// Reject a bad image here, because the decoder rejects it later and a
		// stored session with it could not load.
		if err := checkImage(typed.MediaType, typed.Data); err != nil {
			return fmt.Errorf("anthropic: image block: %w", err)
		}
		type source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		}
		return e.value(struct {
			Type   string `json:"type"`
			Source source `json:"source"`
		}{Type: "image", Source: source{Type: "base64", MediaType: typed.MediaType, Data: typed.Data}})
	case OpaqueBlock:
		return e.value(typed.Raw)
	}
	return fmt.Errorf("anthropic: cannot encode content block of type %T", block)
}

// estimateBytes measures the context size and reports inline images.
// The size includes 32 bytes of overhead plus the content of every block.
func estimateBytes(m Message) (int, bool, error) {
	size := 32
	hasImages := false
	for _, block := range m.Content {
		switch typed := block.(type) {
		case TextBlock:
			size += len(typed.Text)
		case ThinkingBlock:
			size += len(typed.Thinking) + len(typed.Signature)
		case RedactedThinkingBlock:
			size += len(typed.Data)
		case ToolUseBlock:
			size += len(typed.ID) + len(typed.Name) + len(typed.Input)
		case ToolResultBlock:
			size += len(typed.ToolUseID) + len(typed.Content)
		case ImageBlock:
			size += ImageEstimateBytes
			hasImages = true
		case OpaqueBlock:
			size += len(typed.Raw)
		default:
			return 0, false, fmt.Errorf("anthropic: cannot measure content block of type %T", block)
		}
	}
	return size, hasImages, nil
}

// UnmarshalJSON decodes the wire format that MarshalJSON writes. Each block is
// decoded once, without an intermediate copy of its raw JSON. A block of a
// kind this client does not know becomes an OpaqueBlock, so a stored session
// survives new server block kinds.
func (m *Message) UnmarshalJSON(data []byte) error {
	var wire struct {
		Role    Role           `json:"role"`
		Content []blockDecoder `json:"content"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Role != RoleUser && wire.Role != RoleAssistant {
		return fmt.Errorf("anthropic: unknown message role %q", wire.Role)
	}
	content := make([]ContentBlock, len(wire.Content))
	for index, decoded := range wire.Content {
		content[index] = decoded.block
	}
	*m = Message{Role: wire.Role, Content: content}
	return nil
}

type blockDecoder struct {
	block ContentBlock
}

// UnmarshalJSON decodes every known field in one pass. The decoder skips a
// field whose JSON type does not match and reports the first mismatch, so an
// unknown block kind can use the same field names with other types.
func (d *blockDecoder) UnmarshalJSON(raw []byte) error {
	var fields struct {
		Type      string            `json:"type"`
		Text      string            `json:"text"`
		Thinking  string            `json:"thinking"`
		Signature string            `json:"signature"`
		Data      string            `json:"data"`
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Input     json.RawMessage   `json:"input"`
		ToolUseID string            `json:"tool_use_id"`
		Content   resultContentText `json:"content"`
		IsError   bool              `json:"is_error"`
		Source    imageSource       `json:"source"`
	}
	err := json.Unmarshal(raw, &fields)
	if !knownBlock(fields.Type, fields.Source.isBase64) {
		if len(raw) == 0 || raw[0] != '{' {
			return errors.New("content block is not a JSON object")
		}
		d.block = OpaqueBlock{Raw: bytes.Clone(raw)}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s block: %w", fields.Type, err)
	}
	switch fields.Type {
	case "text":
		d.block = TextBlock{Text: fields.Text}
	case "thinking":
		d.block = ThinkingBlock{Thinking: fields.Thinking, Signature: fields.Signature}
	case "redacted_thinking":
		d.block = RedactedThinkingBlock{Data: fields.Data}
	case "tool_use":
		d.block = ToolUseBlock{ID: fields.ID, Name: fields.Name, Input: fields.Input}
	case "tool_result":
		if fields.Content.notString {
			return errors.New("tool_result block: content is not a string")
		}
		d.block = ToolResultBlock{ToolUseID: fields.ToolUseID, Content: fields.Content.text, IsError: fields.IsError}
	case "image":
		if fields.Source.err != nil {
			return fmt.Errorf("image block: %w", fields.Source.err)
		}
		d.block = ImageBlock{MediaType: fields.Source.mediaType, Data: fields.Source.data}
	}
	return nil
}

// knownBlock reports whether a block decodes to a typed block. An image is
// typed only with a base64 source; an image with another source, such as a
// URL, stays opaque so that it goes back to the server unchanged.
func knownBlock(blockType string, base64Source bool) bool {
	switch blockType {
	case "text", "thinking", "redacted_thinking", "tool_use", "tool_result":
		return true
	case "image":
		return base64Source
	default:
		return false
	}
}

// imageSource decodes the "source" field of a block. It never fails, because
// unknown block kinds can use a "source" field of any shape. err holds the
// reason a base64 source is not valid.
type imageSource struct {
	isBase64  bool
	mediaType string
	data      string
	err       error
}

func (s *imageSource) UnmarshalJSON(raw []byte) error {
	*s = imageSource{}
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var fields struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	err := json.Unmarshal(raw, &fields)
	if err == nil {
		err = checkImage(fields.MediaType, fields.Data)
	}
	*s = imageSource{isBase64: fields.Type == "base64", mediaType: fields.MediaType, data: fields.Data, err: err}
	return nil
}

func checkImage(mediaType, data string) error {
	if err := checkImageMediaType(mediaType); err != nil {
		return err
	}
	return checkImageData(data)
}

func checkImageMediaType(mediaType string) error {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return nil
	default:
		return fmt.Errorf("unsupported image media type %q", mediaType)
	}
}

// checkImageData accepts only standard padded base64 without line breaks.
// The library decoder skips line breaks; the check here does not, so that the
// typed decoder and the measuring decoder agree without a decode buffer.
func checkImageData[Text string | []byte](data Text) error {
	if len(data) == 0 {
		return errors.New("image data is empty")
	}
	if len(data)%4 != 0 {
		return errors.New("image data is not padded base64")
	}
	padding := 0
	for index := range len(data) {
		character := data[index]
		switch {
		case 'A' <= character && character <= 'Z', 'a' <= character && character <= 'z',
			'0' <= character && character <= '9', character == '+', character == '/':
			if padding > 0 {
				return errors.New("image data has base64 padding before the end")
			}
		case character == '=':
			if index < len(data)-2 {
				return errors.New("image data has base64 padding before the end")
			}
			padding++
		default:
			return errors.New("image data is not base64")
		}
	}
	return nil
}

// resultContentText accepts a string or null. Unknown block kinds may use a
// "content" field of another JSON type; only tool_result rejects that case.
type resultContentText struct {
	text      string
	notString bool
}

func (c *resultContentText) UnmarshalJSON(raw []byte) error {
	switch {
	case string(raw) == "null":
		return nil
	case len(raw) > 0 && raw[0] == '"':
		return json.Unmarshal(raw, &c.text)
	default:
		c.notString = true
		return nil
	}
}
