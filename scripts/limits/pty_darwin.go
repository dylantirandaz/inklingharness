package main

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPseudoTerminal returns the master side and the path of the slave side.
// It follows posix_openpt, grantpt, unlockpt, and ptsname from libc.
func openPseudoTerminal() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	fd := master.Fd()
	if err := ioctl(fd, syscall.TIOCPTYGRANT, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("grant pseudo-terminal: %w", err)
	}
	if err := ioctl(fd, syscall.TIOCPTYUNLK, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("unlock pseudo-terminal: %w", err)
	}
	// TIOCPTYGNAME writes a NUL-terminated path into a 128-byte buffer.
	var name [128]byte
	if err := ioctl(fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("name pseudo-terminal: %w", err)
	}
	end := bytes.IndexByte(name[:], 0)
	if end < 0 {
		master.Close()
		return nil, "", fmt.Errorf("name pseudo-terminal: name is not terminated")
	}
	return master, string(name[:end]), nil
}
