package lsp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const (
	// maxMessageBytes caps the body of one message from a server, so a broken
	// server cannot fill the memory.
	maxMessageBytes = 32 << 20
	// maxHeaderBytes caps the header part of one message. A real header has
	// fewer than 100 bytes.
	maxHeaderBytes = 4 << 10
)

// frameError tells that the input does not obey the base protocol. After it,
// the reader cannot find the start of the next message, so the stream ends.
type frameError struct {
	reason string
}

func (e *frameError) Error() string {
	return "malformed message frame: " + e.reason
}

// readMessage reads one message and returns its body. It returns io.EOF only
// when the input ends before the first byte of a message.
func readMessage(reader *bufio.Reader) ([]byte, error) {
	length := -1
	headerBytes := 0
	for {
		line, err := reader.ReadSlice('\n')
		headerBytes += len(line)
		switch {
		case errors.Is(err, bufio.ErrBufferFull), headerBytes > maxHeaderBytes:
			return nil, &frameError{reason: "the header is larger than 4 KiB"}
		case errors.Is(err, io.EOF) && headerBytes == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, io.ErrUnexpectedEOF
		case err != nil:
			return nil, err
		}
		// The protocol ends header lines with CRLF. A bare LF is accepted,
		// because it does not make the frame ambiguous.
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(line) == 0 {
			break
		}
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			return nil, &frameError{reason: fmt.Sprintf("the header line %.80q has no colon", line)}
		}
		// Content-Type is the only other header. The body is always UTF-8
		// JSON, so the client does not need it.
		if !bytes.EqualFold(bytes.TrimSpace(name), []byte("Content-Length")) {
			continue
		}
		if length >= 0 {
			return nil, &frameError{reason: "the header has more than one Content-Length"}
		}
		size, err := strconv.ParseUint(string(bytes.TrimSpace(value)), 10, 64)
		if err != nil {
			return nil, &frameError{reason: fmt.Sprintf("Content-Length %.40q is not a decimal number", bytes.TrimSpace(value))}
		}
		if size > maxMessageBytes {
			return nil, &frameError{reason: fmt.Sprintf("the message has %d bytes, more than the 32 MiB limit", size)}
		}
		length = int(size)
	}
	if length < 0 {
		return nil, &frameError{reason: "the header has no Content-Length"}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// writeMessage writes one message. The header and the body go in two writes,
// so the body is not copied. Only one goroutine writes to a stream, so no
// other message can come between them.
func writeMessage(writer io.Writer, body []byte) error {
	header := make([]byte, 0, 40)
	header = append(header, "Content-Length: "...)
	header = strconv.AppendInt(header, int64(len(body)), 10)
	header = append(header, "\r\n\r\n"...)
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := writer.Write(body)
	return err
}
