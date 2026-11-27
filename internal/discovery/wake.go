package discovery

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
)

var wakePorts = []uint16{9, 7}

func MagicPacket(mac string) ([]byte, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, err
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("wake: %q is not a 6-byte MAC", mac)
	}
	return append(bytes.Repeat([]byte{0xFF}, 6), bytes.Repeat(hw, 16)...), nil
}

func Wake(macs []string) error {
	return wake(macs, broadcastAddresses(), wakePorts)
}

func wake(macs []string, addrs []netip.Addr, ports []uint16) error {
	packets := make([][]byte, 0, len(macs))
	for _, m := range macs {
		p, err := MagicPacket(m)
		if err != nil {
			return err
		}
		packets = append(packets, p)
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, a := range addrs {
		for _, port := range ports {
			for _, p := range packets {
				conn.WriteToUDPAddrPort(p, netip.AddrPortFrom(a, port))
			}
		}
	}
	return nil
}
