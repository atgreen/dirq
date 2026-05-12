// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

import (
	"strings"

	"github.com/shirou/gopsutil/v4/net"
)

// NetworkModule collects network interface information.
type NetworkModule struct{}

func (n *NetworkModule) Name() string { return "network" }

func (n *NetworkModule) Collect() (map[string]any, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var interfaces []any
	for _, iface := range ifaces {
		if isLoopback(iface.Name, iface.Flags) {
			continue
		}

		var addrs []any
		for _, addr := range iface.Addrs {
			family := "IPv4"
			if strings.Contains(addr.Addr, ":") {
				family = "IPv6"
			}
			addrs = append(addrs, map[string]any{
				"addr":   addr.Addr,
				"family": family,
			})
		}

		var flags []any
		for _, f := range iface.Flags {
			flags = append(flags, f)
		}

		interfaces = append(interfaces, map[string]any{
			"name":      iface.Name,
			"mac":       iface.HardwareAddr,
			"mtu":       float64(iface.MTU),
			"flags":     flags,
			"addresses": addrs,
		})
	}

	return map[string]any{
		"interfaces": interfaces,
	}, nil
}

// isLoopback returns true if the interface is a loopback device.
func isLoopback(name string, flags []string) bool {
	if strings.HasPrefix(name, "lo") {
		return true
	}
	for _, f := range flags {
		if strings.EqualFold(f, "loopback") {
			return true
		}
	}
	return false
}
