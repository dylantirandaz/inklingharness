package anthropic

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func mustEncode(t *testing.T, messages ...Message) []EncodedMessage {
	t.Helper()
	encoded := make([]EncodedMessage, len(messages))
	for index, message := range messages {
		value, err := EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		encoded[index] = value
	}
	return encoded
}

func everyBlockKind() []Message {
	return []Message{
		{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "read a.txt"}}},
		{Role: RoleAssistant, Content: []ContentBlock{
			ThinkingBlock{Thinking: "plan", Signature: "sig"},
			RedactedThinkingBlock{Data: "opaque-reasoning"},
			ToolUseBlock{ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"a.txt"}`)},
			OpaqueBlock{Raw: json.RawMessage(`{"type":"future_block","payload":{"a":1}}`)},
		}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultBlock{ToolUseID: "toolu_1", Content: "missing", IsError: true}}},
		{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "quotes: \"text\"; slash: \\; line:\n<>&\u2028\u2029"}}},
		{Role: RoleUser, Content: []ContentBlock{ImageBlock{MediaType: "image/png", Data: "iVBORw0KGgo="}, TextBlock{Text: "what is this?"}}},
		{Role: RoleAssistant, Content: []ContentBlock{}},
	}
}

func TestMessageJSONRoundTripKeepsEveryBlockKind(t *testing.T) {
	original := everyBlockKind()
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("round trip changed the messages:\n got %#v\nwant %#v", decoded, original)
	}

	var bad Message
	if err := json.Unmarshal([]byte(`{"role":"system","content":[]}`), &bad); err == nil {
		t.Fatal("unknown role was accepted")
	}
}

func TestMessageJSONKeepsEmptyContentArray(t *testing.T) {
	for _, content := range [][]ContentBlock{nil, {}} {
		encoded, err := json.Marshal(Message{Role: RoleUser, Content: content})
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Content == nil || len(wire.Content) != 0 {
			t.Fatalf("empty content must be a JSON array: %s", encoded)
		}
	}
}

func TestMessageJSONRejectsMalformedBlocks(t *testing.T) {
	for _, block := range []ContentBlock{
		OpaqueBlock{Raw: json.RawMessage(`{"type":`)},
		ToolUseBlock{ID: "call", Name: "read_file", Input: json.RawMessage(`{"path":`)},
		ImageBlock{MediaType: "image/bmp", Data: "AAAA"},
		ImageBlock{MediaType: "image/png", Data: "AAA"},
		ImageBlock{MediaType: "image/png", Data: ""},
		ImageBlock{MediaType: "image/png", Data: "AA\nA"},
		nil,
	} {
		message := Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "before invalid block"}, block}}
		if _, err := json.Marshal(message); err == nil {
			t.Fatalf("invalid block accepted: %#v", block)
		}
	}
}

func TestEncodedMessageKeepsCompactWireAndEstimate(t *testing.T) {
	for _, message := range everyBlockKind() {
		encoded, err := EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		marshaled, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encoded.Wire(), marshaled) || bytes.ContainsRune(encoded.Wire(), '\n') {
			t.Fatalf("wire is not compact json.Marshal output:\n got %s\nwant %s", encoded.Wire(), marshaled)
		}
		parsed, err := ParseEncodedMessage(bytes.Clone(encoded.Wire()))
		if err != nil {
			t.Fatal(err)
		}
		if !parsed.Equal(encoded) || parsed.EstimateBytes() != encoded.EstimateBytes() || parsed.Role() != message.Role {
			t.Fatalf("parse changed metadata: %+v, want %+v", parsed, encoded)
		}
		decoded, err := parsed.Decode()
		if err != nil || !reflect.DeepEqual(decoded, message) {
			t.Fatalf("decode = %#v, %v; want %#v", decoded, err, message)
		}
	}
	text := Message{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "abcd"}, ToolResultBlock{ToolUseID: "id", Content: "xyz"}}}
	if encoded := mustEncode(t, text)[0]; encoded.EstimateBytes() != 32+4+2+3 {
		t.Fatalf("estimate bytes = %d", encoded.EstimateBytes())
	}
	image := Message{Role: RoleUser, Content: []ContentBlock{ImageBlock{MediaType: "image/webp", Data: strings.Repeat("A", 4000)}}}
	if encoded := mustEncode(t, image)[0]; encoded.EstimateBytes() != 32+imageEstimateBytes {
		t.Fatalf("image estimate bytes = %d", encoded.EstimateBytes())
	}
	if _, err := EncodeMessage(Message{Role: "system"}); err == nil {
		t.Fatal("unknown role was encoded")
	}
	if _, err := (EncodedMessage{}).Decode(); err == nil {
		t.Fatal("zero encoded message decoded")
	}
}

func TestMessageDecodingKeepsUnknownBlocksAndRejectsBadKnownBlocks(t *testing.T) {
	future := `{"type":"future","content":[1,2],"text":7,"input":{"a":1}}`
	parsed, err := ParseEncodedMessage([]byte(`{"role":"assistant","content":[` + future + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := parsed.Decode()
	if err != nil {
		t.Fatal(err)
	}
	opaque, ok := decoded.Content[0].(OpaqueBlock)
	if !ok || string(opaque.Raw) != future || parsed.EstimateBytes() != 32+len(future) {
		t.Fatalf("unknown block = %#v, estimate %d", decoded.Content[0], parsed.EstimateBytes())
	}
	for _, wire := range []string{
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"x"}]}]}`,
		`{"role":"user","content":[{"type":"text","text":7}]}`,
		`{"role":"user","content":[7]}`,
		`{"role":"system","content":[]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/bmp","data":"AAAA"}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA*A"}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A=AA"}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]}`,
	} {
		if _, err := ParseEncodedMessage([]byte(wire)); err == nil {
			t.Errorf("invalid message accepted: %s", wire)
		}
		var typed Message
		if err := typed.UnmarshalJSON([]byte(wire)); err == nil {
			t.Errorf("typed decoder accepted invalid message: %s", wire)
		}
	}
	// An image with a URL source is not a typed image. It must go back
	// unchanged, with its key order and unknown fields.
	url := `{"source":{"url":"https://example.com/a.png","type":"url","extra":[1]},"type":"image","cache_control":{"type":"ephemeral"}}`
	parsed, err = ParseEncodedMessage([]byte(`{"role":"user","content":[` + url + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = parsed.Decode()
	if err != nil {
		t.Fatal(err)
	}
	opaque, ok = decoded.Content[0].(OpaqueBlock)
	if !ok || string(opaque.Raw) != url || parsed.EstimateBytes() != 32+len(url) {
		t.Fatalf("url image = %#v, estimate %d", decoded.Content[0], parsed.EstimateBytes())
	}
	reencoded := mustEncode(t, decoded)[0]
	if !bytes.Contains(reencoded.Wire(), []byte(url)) {
		t.Fatalf("url image changed on encode: %s", reencoded.Wire())
	}
	null, err := ParseEncodedMessage([]byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := null.Decode(); err != nil || decoded.Content[0] != (ToolResultBlock{ToolUseID: "a"}) {
		t.Fatalf("null tool result content = %#v, %v", decoded, err)
	}
	if strings.Contains(string(mustEncode(t, Message{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "<&>"}}})[0].Wire()), "<") {
		t.Fatal("HTML characters must stay escaped as json.Marshal escapes them")
	}
}

// The load path measures messages without decoding them. It must accept,
// reject, and size exactly as the typed decoder does, or the compaction
// trigger and corrupt-session detection change after a resume.
func TestParseMatchesTypedDecoding(t *testing.T) {
	text := "\"plain \\n \\\" \\\\ \\/ \\u00e9 \\u20ac \\ud83d\\ude00 lone \\ud800 end \\udc00\\u0041 raw \xff\xfe é 😀\""
	for _, wire := range []string{
		`{"role":"user","content":[{"type":"text","text":` + text + `}]}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":` + text + `,"signature":"s\u0069g"},{"type":"redacted_thinking","data":` + text + `}]}`,
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file","input":{ "path" : "a.txt" }},{"type":"tool_use","id":"t2","name":"n","input":null},{"type":"tool_use","id":"t3","name":"n"}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + text + `,"is_error":true},{"type":"tool_result","tool_use_id":"t2","content":null}]}`,
		`{"role":"assistant","content":[{"type":"future","content":[1],"text":7,"is_error":"x"},{"no_type":true}]}`,
		`{"role":"user","content":null}`,
		`{"role":"user"}`,
		`{"role":"user","content":[{"type":"text","text":7}]}`,
		`{"role":"user","content":[{"type":"text","text":"x","id":7}]}`,
		`{"role":"user","content":[{"type":"text","text":"x","is_error":"yes"}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text"}]}]}`,
		`{"role":"user","content":[7]}`,
		`{"role":"system","content":[]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"R0lG\u0042\/w="}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"R0lGOD=="},"text":"x"}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/","data":"/9j/"}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}`,
		`{"role":"user","content":[{"type":"image","source":"text"},{"type":"image","source":null},{"type":"image"}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":7,"data":7}}]}`,
		`{"role":"user","content":[{"type":"text","text":"x","source":{"type":"base64","data":"!"}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"},"text":7}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":7}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":7,"data":"AAAA"}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"!!!!","data":"AAAA"}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA","data":null}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":null}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA\nAA=="}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"\u00e9AAA"}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":7,"data":"AAAA"}}]}`,
		`{"role":"user","content":[{"type":"text","text":"unterminated}]}`,
	} {
		var typed Message
		typedErr := typed.UnmarshalJSON([]byte(wire))
		parsed, parseErr := ParseEncodedMessage([]byte(wire))
		if (typedErr == nil) != (parseErr == nil) {
			t.Errorf("typed error %v, parse error %v for %s", typedErr, parseErr, wire)
			continue
		}
		if typedErr != nil {
			continue
		}
		want, err := estimateBytes(typed)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.EstimateBytes() != want || parsed.Role() != typed.Role {
			t.Errorf("estimate %d role %q, want %d %q for %s", parsed.EstimateBytes(), parsed.Role(), want, typed.Role, wire)
		}
	}
}
