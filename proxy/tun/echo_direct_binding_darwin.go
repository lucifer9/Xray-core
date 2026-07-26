//go:build darwin

package tun

import (
	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/sys/unix"
)

func bindDarwinDirectEchoSocket(family echoFamily, fd uintptr, carrier carrierInterface, setOption func(int, int, int, int) error) error {
	var err error
	if family == echoIPv4 {
		err = setOption(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, carrier.Index)
	} else {
		err = setOption(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, carrier.Index)
	}
	if err != nil {
		return errors.New("failed to bind Direct Echo socket to Outbound carrier interface ", carrier.Name).Base(err)
	}
	return nil
}
