//go:build darwin || linux

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal checks terminal support without starting a child process.
func IsTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	_, err := getState(file)
	return err == nil
}

func getState(file *os.File) (syscall.Termios, error) {
	var state syscall.Termios
	_, _, code := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), getStateRequest, uintptr(unsafe.Pointer(&state)))
	if code != 0 {
		return syscall.Termios{}, fmt.Errorf("read terminal state: %w", code)
	}
	return state, nil
}

func setState(file *os.File, state *syscall.Termios) error {
	_, _, code := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), setStateRequest, uintptr(unsafe.Pointer(state)))
	if code != 0 {
		return fmt.Errorf("set terminal state: %w", code)
	}
	return nil
}

func rawState(state syscall.Termios) syscall.Termios {
	state.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	state.Oflag &^= syscall.OPOST
	state.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	state.Cflag &^= syscall.CSIZE | syscall.PARENB
	state.Cflag |= syscall.CS8
	state.Cc[syscall.VMIN], state.Cc[syscall.VTIME] = 1, 0
	return state
}

func terminalSize(file *os.File) (int, int, error) {
	// The kernel winsize layout has four unsigned 16-bit fields on both hosts.
	var size struct{ rows, columns, horizontalPixels, verticalPixels uint16 }
	_, _, code := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	if code != 0 {
		return 0, 0, fmt.Errorf("read terminal size: %w", code)
	}
	if size.rows < 1 || size.columns < 2 {
		return 0, 0, fmt.Errorf("invalid terminal size %dx%d", size.rows, size.columns)
	}
	return int(size.rows), int(size.columns), nil
}
