//go:build linux && !android

package tun

import (
	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/sys/unix"
)

func bindDirectEchoSocket(_ echoFamily, fd uintptr, carrier carrierInterface) error {
	return bindLinuxDirectEchoSocket(fd, carrier, unix.BindToDevice)
}

func bindLinuxDirectEchoSocket(fd uintptr, carrier carrierInterface, bind func(int, string) error) error {
	if err := bind(int(fd), carrier.Name); err != nil {
		return errors.New("failed to bind Direct Echo socket to Outbound carrier interface ", carrier.Name).Base(err)
	}
	return nil
}
