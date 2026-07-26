//go:build darwin && !ios

package tun

import "golang.org/x/sys/unix"

func bindDirectEchoSocket(family echoFamily, fd uintptr, carrier carrierInterface) error {
	return bindDarwinDirectEchoSocket(family, fd, carrier, unix.SetsockoptInt)
}
