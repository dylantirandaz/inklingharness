package terminal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// backgroundQuery asks for the background color (OSC 11) and then for the
// primary device attributes (DA1). Terminals answer in order, and every
// terminal answers DA1, so its reply ends the wait at once on a terminal that
// ignores OSC 11.
const backgroundQuery = "\x1b]11;?\x1b\\\x1b[c"

// backgroundTimeout bounds the wait for a terminal that answers neither. A
// reply that comes later is dropped by the input parser, so a short wait
// costs only the shade, never typed text.
const backgroundTimeout = 200 * time.Millisecond

// maxReplyBytes bounds an unterminated OSC reply held by the input parser.
const maxReplyBytes = 512

// queryShade returns the shade of the terminal background and the bytes that
// belong to neither reply, which are typed input. The terminal must be in raw
// mode, or its replies would be echoed and held for a line. A terminal that
// does not report its background is taken as dark, the common default.
func queryShade(input, output *os.File, wake int) (presentation.Shade, []byte, error) {
	if _, err := io.WriteString(output, backgroundQuery); err != nil {
		return presentation.DarkBackground, nil, fmt.Errorf("terminal background query: %w", err)
	}
	deadline := time.Now().Add(backgroundTimeout)
	var received []byte
	var data [256]byte
	for {
		if replies, rest, found := cutDeviceAttributes(received); found {
			shade, reported := backgroundShade(replies)
			if !reported {
				shade = presentation.DarkBackground
			}
			return shade, rest, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return presentation.DarkBackground, received, nil
		}
		ready, _, err := waitReadable(int(input.Fd()), wake, remaining)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return presentation.DarkBackground, nil, fmt.Errorf("terminal background query: %w", err)
		}
		if !ready {
			continue
		}
		n, err := syscall.Read(int(input.Fd()), data[:])
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return presentation.DarkBackground, nil, fmt.Errorf("terminal background query: %w", err)
		}
		if n == 0 {
			return presentation.DarkBackground, nil, fmt.Errorf("terminal background query: %w", io.EOF)
		}
		received = append(received, data[:n]...)
	}
}

// cutDeviceAttributes finds the DA1 reply, ESC [ ? params c. It returns the
// bytes up to and including that reply, which hold any OSC 11 reply, and the
// typed input around them.
func cutDeviceAttributes(received []byte) (replies, rest []byte, found bool) {
	for start := 0; start < len(received); {
		index := bytes.Index(received[start:], []byte("\x1b[?"))
		if index < 0 {
			return nil, nil, false
		}
		index += start
		end := index + 3
		for end < len(received) && (received[end] == ';' || (received[end] >= '0' && received[end] <= '9')) {
			end++
		}
		if end < len(received) && received[end] == 'c' {
			// Copy the typed bytes: received still holds the replies to parse.
			rest = append(removeBackgroundReply(slices.Clone(received[:index])), received[end+1:]...)
			return received[:end+1], rest, true
		}
		start = index + 1
	}
	return nil, nil, false
}

// removeBackgroundReply drops an OSC 11 reply from typed input.
func removeBackgroundReply(text []byte) []byte {
	start := bytes.Index(text, []byte("\x1b]11;"))
	if start < 0 {
		return text
	}
	end, length := terminatorOf(text[start:])
	if end < 0 {
		return text[:start]
	}
	return append(text[:start:start], text[start+end+length:]...)
}

// terminatorOf finds the end of an OSC string: BEL or ESC \.
func terminatorOf(text []byte) (index, length int) {
	bell := bytes.IndexByte(text, '\a')
	st := bytes.Index(text, []byte("\x1b\\"))
	switch {
	case bell >= 0 && (st < 0 || bell < st):
		return bell, 1
	case st >= 0:
		return st, 2
	default:
		return -1, 0
	}
}

// backgroundShade parses "ESC ] 11 ; rgb:R/G/B" with 1 to 4 hex digits for
// each channel.
func backgroundShade(replies []byte) (presentation.Shade, bool) {
	start := bytes.Index(replies, []byte("\x1b]11;rgb:"))
	if start < 0 {
		return presentation.DarkBackground, false
	}
	body := replies[start+len("\x1b]11;rgb:"):]
	end, _ := terminatorOf(body)
	if end < 0 {
		return presentation.DarkBackground, false
	}
	parts := bytes.Split(body[:end], []byte("/"))
	if len(parts) != 3 {
		return presentation.DarkBackground, false
	}
	var channels [3]uint8
	for index, part := range parts {
		if len(part) < 1 || len(part) > 4 {
			return presentation.DarkBackground, false
		}
		value, err := strconv.ParseUint(string(part), 16, 16)
		if err != nil {
			return presentation.DarkBackground, false
		}
		maximum := uint64(1)<<(4*len(part)) - 1
		channels[index] = uint8((value*255 + maximum/2) / maximum)
	}
	return presentation.ShadeOf(channels[0], channels[1], channels[2]), true
}
