package agentcfg

import (
	"net"
	"os"
)

// containerMarkers are the files podman and docker place in a container's
// filesystem; their absence means picolet runs directly on the Machine.
var containerMarkers = []string{"/run/.containerenv", "/.dockerenv"}

// podmanPrivatePools are the address pools podman hands to a container that has
// a network namespace of its own: the netavark/CNI bridge (rootful) and pasta or
// slirp4netns (rootless). A Machine's own NICs carry addresses outside them —
// its bridge gateway (10.88.0.1) lives on podman0, which is accompanied by the
// real interfaces in the host namespace.
var podmanPrivatePools = []net.IPNet{
	{IP: net.IPv4(10, 88, 0, 0), Mask: net.CIDRMask(16, 32)},
	{IP: net.IPv4(10, 0, 2, 0), Mask: net.CIDRMask(24, 32)},
}

// InPrivateNetworkNamespace reports whether picolet looks like it runs
// containerized in a network namespace of its own, i.e. without Network=host,
// where a loopback bind is invisible to the Machine.
//
// The answer is a heuristic in both directions: only podman's default private
// pools are recognized, so a container on a custom network reads false, and a
// Network=host agent on a Machine whose only NIC sits inside one of those pools
// reads true. Use it to phrase a conditional warning, never to gate behaviour.
func InPrivateNetworkNamespace() bool {
	if !inContainer() {
		return false
	}
	return privateNetworkNamespace(routableAddrs())
}

func inContainer() bool {
	for _, marker := range containerMarkers {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// privateNetworkNamespace reports whether every routable address belongs to a
// podman private pool. An empty list is inconclusive: a namespace without any
// routable address may equally be a host namespace before DHCP.
func privateNetworkNamespace(addrs []net.IP) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, ip := range addrs {
		if !inPrivatePool(ip) {
			return false
		}
	}
	return true
}

func inPrivatePool(ip net.IP) bool {
	for i := range podmanPrivatePools {
		if podmanPrivatePools[i].Contains(ip) {
			return true
		}
	}
	return false
}

// routableAddrs returns the addresses of every up, non-loopback interface,
// skipping link-local addresses, which say nothing about reachability.
func routableAddrs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var addrs []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range ifaceAddrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLinkLocalUnicast() || ipNet.IP.IsLoopback() {
				continue
			}
			addrs = append(addrs, ipNet.IP)
		}
	}
	return addrs
}
