// Package netinfo finds this host's LAN address and subnet.
package netinfo

import (
	"errors"
	"net"
)

// LAN returns the IPv4 address used for the default route and its subnet
// (e.g. 192.168.1.10, 192.168.1.0/24). No packets are sent.
func LAN() (ip net.IP, subnet *net.IPNet, err error) {
	conn, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1; only selects a route
	if err != nil {
		return nil, nil, err
	}
	ip = conn.LocalAddr().(*net.UDPAddr).IP.To4()
	conn.Close()

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ip, nil, err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return ip, &net.IPNet{IP: ip.Mask(n.Mask), Mask: n.Mask}, nil
		}
	}
	return ip, nil, errors.New("could not find subnet for " + ip.String())
}
