package terminal

import (
	"syscall"
	"time"
)

func inputReady(fd int, timeout time.Duration) (bool, error) {
	set := descriptorSet(fd)
	interval := syscall.NsecToTimeval(timeout.Nanoseconds())
	if err := syscall.Select(fd+1, &set, nil, nil, &interval); err != nil {
		return false, err
	}
	return isSet(&set, fd), nil
}

// waitReadable blocks until input or wake is readable. A negative timeout
// waits without a limit.
func waitReadable(input, wake int, timeout time.Duration) (inputReady, woken bool, err error) {
	set := descriptorSet(input)
	addDescriptor(&set, wake)
	var interval *syscall.Timeval
	if timeout >= 0 {
		value := syscall.NsecToTimeval(timeout.Nanoseconds())
		interval = &value
	}
	if err := syscall.Select(max(input, wake)+1, &set, nil, nil, interval); err != nil {
		return false, false, err
	}
	return isSet(&set, input), isSet(&set, wake), nil
}
