package network

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const maxEthernetFrame = 65536

// ethernetConn validates before gvproxy allocates a frame or learns its source.
// The bounded writer prevents a slow guest from blocking the entire switch.
type ethernetConn struct {
	net.Conn
	ip      net.IP
	mac     net.HardwareAddr
	frame   [maxEthernetFrame + 4]byte
	pending []byte
	writes  chan []byte
	done    chan struct{}
	once    sync.Once
}

func newEthernetConn(c net.Conn, ip, mac string) *ethernetConn {
	hw, _ := net.ParseMAC(mac)
	e := &ethernetConn{Conn: c, ip: net.ParseIP(ip).To4(), mac: hw, writes: make(chan []byte, 64), done: make(chan struct{})}
	go e.writer()
	return e
}
func (e *ethernetConn) Close() error {
	var err error
	e.once.Do(func() { close(e.done); err = e.Conn.Close() })
	return err
}
func (e *ethernetConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for len(e.pending) == 0 {
		// Idle links may stay open indefinitely; incomplete frames have a deadline.
		e.Conn.SetReadDeadline(time.Time{})
		if _, err := io.ReadFull(e.Conn, e.frame[:1]); err != nil {
			return 0, err
		}
		e.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(e.Conn, e.frame[1:4]); err != nil {
			return 0, err
		}
		size := binary.BigEndian.Uint32(e.frame[:4])
		if size < 14 || size > maxEthernetFrame {
			return 0, fmt.Errorf("invalid Ethernet frame length %d", size)
		}
		if _, err := io.ReadFull(e.Conn, e.frame[4:4+size]); err != nil {
			return 0, err
		}
		if !e.validSource(e.frame[4 : 4+size]) {
			continue
		}
		e.pending = e.frame[:4+size]
	}
	n := copy(b, e.pending)
	e.pending = e.pending[n:]
	return n, nil
}
func (e *ethernetConn) validSource(f []byte) bool {
	if len(e.mac) == 0 {
		return true
	} // generic test/legacy endpoint
	if len(f) < 14 || !bytes.Equal(f[6:12], e.mac) {
		return false
	}
	switch binary.BigEndian.Uint16(f[12:14]) {
	case 0x0800:
		return len(f) >= 34 && f[14]>>4 == 4 && int(f[14]&15)*4 >= 20 && len(f) >= 14+int(f[14]&15)*4 && bytes.Equal(f[26:30], e.ip)
	case 0x0806:
		return len(f) >= 42 && binary.BigEndian.Uint16(f[14:16]) == 1 && binary.BigEndian.Uint16(f[16:18]) == 0x0800 && f[18] == 6 && f[19] == 4 && bytes.Equal(f[22:28], e.mac) && (bytes.Equal(f[28:32], e.ip) || bytes.Equal(f[28:32], []byte{0, 0, 0, 0}))
	default:
		return false // IPv6 and VLAN trunks are not part of this IPv4 contract.
	}
}
func (e *ethernetConn) Write(b []byte) (int, error) {
	if len(b) < 18 || len(b) > maxEthernetFrame+4 || int(binary.BigEndian.Uint32(b[:4])) != len(b)-4 {
		return 0, fmt.Errorf("invalid outgoing frame")
	}
	select {
	case <-e.done:
		return 0, net.ErrClosed
	default:
	}
	select {
	case e.writes <- append([]byte(nil), b...):
	default:
	} // drop whole frames under backpressure
	return len(b), nil
}
func (e *ethernetConn) writer() {
	defer e.Close()
	for {
		select {
		case <-e.done:
			return
		case b := <-e.writes:
			e.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			for len(b) > 0 {
				n, err := e.Conn.Write(b)
				if err != nil || n == 0 {
					return
				}
				b = b[n:]
			}
		}
	}
}
