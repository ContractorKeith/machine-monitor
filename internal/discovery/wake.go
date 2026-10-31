package discovery

// Wake-on-LAN, ported from Forward-Future/device-monitor discovery.js (wake) at c3a4c41.
// MIT License, Copyright (c) 2026 Matt: https://github.com/Forward-Future/device-monitor

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
)

// wakePorts are the usual WoL ports, as upstream.
var wakePorts = []uint16{9, 7}

// MagicPacket builds the 102-byte WoL packet for mac: 6 x 0xFF, then the MAC 16 times.
// mac may use ':' or '-' separators; only 6-byte (EUI-48) addresses are accepted.
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

// Wake broadcasts a magic packet for each MAC on every local IPv4 network, on ports 9 and 7.
// The target's network card and firmware must have WoL turned on. Every MAC is validated
// before anything is sent; send errors on individual networks are ignored, as upstream.
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
