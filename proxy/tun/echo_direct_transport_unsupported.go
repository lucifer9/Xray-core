//go:build (!linux && !darwin) || android || ios

package tun

func openEchoPacketIO(echoFamily, carrierInterface) (echoPacketIO, error) {
	return nil, errDirectEchoUnsupported
}
