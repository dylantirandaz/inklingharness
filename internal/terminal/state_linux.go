package terminal

import "syscall"

const (
	getStateRequest = syscall.TCGETS
	setStateRequest = syscall.TCSETS
)
