package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPseudoTerminal returns the master side and the path of the slave side,
// as posix_openpt, grantpt, unlockpt, and ptsname do.
func openPseudoTerminal() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	fd := master.Fd()
	var name [128]byte
	if err := errors.Join(
		ioctl(fd, syscall.TIOCPTYGRANT, 0),
		ioctl(fd, syscall.TIOCPTYUNLK, 0),
		ioctl(fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))),
	); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("pseudo-terminal: %w", err)
	}
	end := bytes.IndexByte(name[:], 0)
	if end < 0 {
		master.Close()
		return nil, "", errors.New("pseudo-terminal name is not terminated")
	}
	return master, string(name[:end]), nil
}
