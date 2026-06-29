//go:build (!linux && !darwin) || android

package tun

import "github.com/xtls/xray-core/common/errors"

func NewRouteManager(tunName string, tunIndex int) (RouteManager, error) {
	return nil, errors.New("auto_route is not supported on this platform")
}
