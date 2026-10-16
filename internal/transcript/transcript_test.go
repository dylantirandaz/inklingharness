package transcript

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/session"
)

func TestWriteEmbedsSessionSafely(t *testing.T) {
	prompt, err := anthropic.EncodeMessage(anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "close </script><img src=x onerror=alert(1)> & go"}}})
	if err != nil {
		t.Fatal(err)
	}
	value := session.Session{ID: "abc", Title: "Demo", Model: "deepseek/deepseek-v4.1-flash", Messages: []anthropic.EncodedMessage{prompt}}
	var page bytes.Buffer
	if err := Write(&page, value, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	if strings.Count(html, "</script>") != 2 {
		t.Fatalf("session text must not close a script element early")
	}
	start := strings.Index(html, `type="application/json">`) + len(`type="application/json">`)
	end := strings.Index(html[start:], "</script>")
	var decoded struct {
		Title    string            `json:"title"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(html[start:start+end]), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Title != "Demo" || len(decoded.Messages) != 1 {
		t.Fatalf("embedded data did not round-trip: %+v", decoded)
	}
	var message struct {
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal(decoded.Messages[0], &message); err != nil || len(message.Content) != 1 || !strings.Contains(message.Content[0].Text, "</script>") {
		t.Fatalf("embedded data did not round-trip: %+v", decoded)
	}
	if strings.Contains(html, "innerHTML") {
		t.Fatal("the viewer must build the page from DOM nodes")
	}
}
