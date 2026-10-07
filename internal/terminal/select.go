package terminal

import (
	"fmt"
	"syscall"
	"unsafe"
)

func checkDescriptor(fd int) error {
	var set syscall.FdSet
	capacity := len(set.Bits) * int(unsafe.Sizeof(set.Bits[0])) * 8
	if fd < 0 || fd >= capacity {
		return fmt.Errorf("terminal descriptor %d exceeds select capacity %d", fd, capacity)
	}
	return nil
}

func descriptorSet(fd int) syscall.FdSet {
	var set syscall.FdSet
	addDescriptor(&set, fd)
	return set
}

func addDescriptor(set *syscall.FdSet, fd int) {
	bits := int(unsafe.Sizeof(set.Bits[0])) * 8
	set.Bits[fd/bits] |= 1 << uint(fd%bits)
}

func isSet(set *syscall.FdSet, fd int) bool {
	bits := int(unsafe.Sizeof(set.Bits[0])) * 8
	return set.Bits[fd/bits]&(1<<uint(fd%bits)) != 0
}
