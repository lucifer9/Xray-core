//go:build !linux || android

package tun

func enableICMPBypassRouting(_ int) error {
	return nil
}
