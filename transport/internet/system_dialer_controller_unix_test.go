//go:build darwin || linux

package internet

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func setTestSocketOptionInt(c syscall.RawConn, level, option, value int) error {
	var optionErr error
	if err := c.Control(func(fd uintptr) {
		optionErr = unix.SetsockoptInt(int(fd), level, option, value)
	}); err != nil {
		return err
	}
	return optionErr
}

func getTestSocketOptionInt(c syscall.RawConn, level, option int) (int, error) {
	var value int
	var optionErr error
	if err := c.Control(func(fd uintptr) {
		value, optionErr = unix.GetsockoptInt(int(fd), level, option)
	}); err != nil {
		return 0, err
	}
	return value, optionErr
}
