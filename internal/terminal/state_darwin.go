package terminal

import "syscall"

const (
	getStateRequest = syscall.TIOCGETA
	setStateRequest = syscall.TIOCSETA
)
