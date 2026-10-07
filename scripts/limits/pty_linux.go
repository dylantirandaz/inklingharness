package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// openPseudoTerminal returns the master side and the path of the slave side.
// Linux grants the slave at open time, so only the unlock is necessary.
func openPseudoTerminal() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	fd := master.Fd()
	var unlock int32
	if err := ioctl(fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("unlock pseudo-terminal: %w", err)
	}
	var number uint32
	if err := ioctl(fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("number pseudo-terminal: %w", err)
	}
	return master, "/dev/pts/" + strconv.FormatUint(uint64(number), 10), nil
}
