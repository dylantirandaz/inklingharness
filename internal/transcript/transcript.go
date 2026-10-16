// Package transcript writes a session as one self-contained HTML page: the
// conversation as JSON plus an embedded viewer script. Nothing here runs
// during a turn; /export calls it once, and the page does its work in the
// browser that opens it.
package transcript

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/session"
)

//go:embed viewer.html
var viewer string

// dataMarker is replaced by the session JSON inside a
// <script type="application/json"> element.
const dataMarker = "/*SESSION_DATA*/"

type page struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Model     string            `json:"model"`
	Effort    string            `json:"effort"`
	WorkDir   string            `json:"work_dir"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Usage     anthropic.Usage   `json:"usage"`
	Exported  time.Time         `json:"exported_at"`
	Messages  []json.RawMessage `json:"messages"`
}

// Write renders value as HTML to output.
func Write(output io.Writer, value session.Session, now time.Time) error {
	data := page{
		ID: value.ID, Title: value.Title, Model: value.Model, Effort: value.Effort, WorkDir: value.WorkDir,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, Usage: value.Usage, Exported: now,
		Messages: make([]json.RawMessage, 0, len(value.Messages)),
	}
	for index, message := range value.Messages {
		wire := message.Wire()
		if !json.Valid(wire) {
			return fmt.Errorf("transcript: message %d is not valid JSON", index)
		}
		data.Messages = append(data.Messages, json.RawMessage(wire))
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("transcript: encode: %w", err)
	}
	// Stored messages may hold a literal "</script>"; escaping <, >, & and
	// the JavaScript line separators keeps the data inside its element.
	var escaped bytes.Buffer
	json.HTMLEscape(&escaped, encoded)
	before, after, found := strings.Cut(viewer, dataMarker)
	if !found {
		return fmt.Errorf("transcript: viewer has no data marker")
	}
	if _, err := io.WriteString(output, before); err != nil {
		return err
	}
	if _, err := output.Write(escaped.Bytes()); err != nil {
		return err
	}
	_, err = io.WriteString(output, after)
	return err
}
