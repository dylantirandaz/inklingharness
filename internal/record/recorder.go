// Package record stores each request body and raw reply stream on disk.
package record

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// DirectoryRecorder writes NNNN.request.json and NNNN.response.sse pairs into
// one directory, numbered in call order.
type DirectoryRecorder struct {
	directory string
	mutex     sync.Mutex
	count     int
}

// NewDirectoryRecorder creates the directory when it is missing.
func NewDirectoryRecorder(directory string) (*DirectoryRecorder, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("record: create %s: %w", directory, err)
	}
	return &DirectoryRecorder{directory: directory}, nil
}

// Begin writes the request body segments in order and opens the reply file.
func (r *DirectoryRecorder) Begin(requestBody [][]byte) (io.WriteCloser, error) {
	r.mutex.Lock()
	r.count++
	sequence := r.count
	r.mutex.Unlock()

	requestPath := filepath.Join(r.directory, fmt.Sprintf("%04d.request.json", sequence))
	request, err := os.OpenFile(requestPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("record: create %s: %w", requestPath, err)
	}
	segments := net.Buffers(slices.Clone(requestBody))
	_, writeErr := segments.WriteTo(request)
	if err := errors.Join(writeErr, request.Close()); err != nil {
		return nil, fmt.Errorf("record: write %s: %w", requestPath, err)
	}
	responsePath := filepath.Join(r.directory, fmt.Sprintf("%04d.response.sse", sequence))
	response, err := os.Create(responsePath)
	if err != nil {
		return nil, fmt.Errorf("record: create %s: %w", responsePath, err)
	}
	return response, nil
}
