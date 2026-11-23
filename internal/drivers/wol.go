package drivers

import (
	"context"
	"fmt"
	"net"
	"strconv"
)

// WakeOnLAN sends magic packets.
type WakeOnLAN struct {
	MAC       net.HardwareAddr
	Broadcast string
	Port      int
}

// NewWakeOnLAN validates the MAC and fills defaults.
func NewWakeOnLAN(mac, broadcast string, port int) (*WakeOnLAN, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("wake-on-lan: %w", err)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("wake-on-lan: MAC %q is not 6 bytes", mac)
	}
	if broadcast == "" {
		broadcast = "255.255.255.255"
	}
	if port == 0 {
		port = 9
	}
	return &WakeOnLAN{MAC: hw, Broadcast: broadcast, Port: port}, nil
}

// MagicPacket builds the 102-byte payload: 6×0xFF then the MAC 16 times.
func MagicPacket(mac net.HardwareAddr) []byte {
	p := make([]byte, 0, 102)
	for range 6 {
		p = append(p, 0xff)
	}
	for range 16 {
		p = append(p, mac...)
	}
	return p
}

// On sends the magic packet a few times (UDP is lossy).
func (w *WakeOnLAN) On(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp4", net.JoinHostPort(w.Broadcast, strconv.Itoa(w.Port)))
	if err != nil {
		return fmt.Errorf("wake-on-lan dial: %w", err)
	}
	defer conn.Close()
	pkt := MagicPacket(w.MAC)
	for range 3 {
		if _, err := conn.Write(pkt); err != nil {
			return fmt.Errorf("wake-on-lan send: %w", err)
		}
	}
	return nil
}
