package network

import (
	"context"
	"encoding/binary"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"testing"
	"time"
)

func TestEthernetRejectsUnboundedFrames(t *testing.T) {
	for _, size := range []uint32{0, 13, 65537, 0xffffffff} {
		a, b := net.Pipe()
		c := newEthernetConn(a, "", "")
		done := make(chan error, 1)
		go func() { var dst [20]byte; _, err := c.Read(dst[:]); done <- err }()
		var h [4]byte
		binary.BigEndian.PutUint32(h[:], size)
		_, err := b.Write(h[:])
		require.NoError(t, err)
		require.Error(t, <-done)
		c.Close()
		b.Close()
	}
}
func TestEthernetSourceBinding(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newEthernetConn(a, "172.20.0.2", "02:01:02:03:04:05")
	defer c.Close()
	frame := make([]byte, 34)
	copy(frame[6:12], c.mac)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	frame[14] = 0x45
	copy(frame[26:30], c.ip)
	require.True(t, c.validSource(frame))
	frame[29]++
	require.False(t, c.validSource(frame))
	frame[29]--
	frame[6]++
	require.False(t, c.validSource(frame))
	frame[6]--
	frame[12] = 0x86
	frame[13] = 0xdd
	require.False(t, c.validSource(frame))
}
func TestEthernetSplitFrameAndSlowWriter(t *testing.T) {
	a, b := net.Pipe()
	c := newEthernetConn(a, "", "")
	defer c.Close()
	defer b.Close()
	frame := make([]byte, 18)
	frame[3] = 14
	done := make(chan []byte, 1)
	go func() {
		got := make([]byte, 18)
		_, err := io.ReadFull(c, got)
		if err != nil {
			done <- nil
		} else {
			done <- got
		}
	}()
	for _, x := range frame {
		_, err := b.Write([]byte{x})
		require.NoError(t, err)
	}
	require.Equal(t, frame, <-done)
	// A stalled switch consumer cannot block another VM or grow without bound.
	start := time.Now()
	for range 1000 {
		n, err := c.Write(frame)
		require.NoError(t, err)
		require.Equal(t, len(frame), n)
	}
	require.Less(t, time.Since(start), time.Second)
	require.LessOrEqual(t, len(c.writes), ethernetQueueFrames)
}
func TestNativeFailedDialReturnsNilInterface(t *testing.T) {
	n, err := NewNativeNetwork("172.29.0.0/24", "172.29.0.1", nil)
	require.NoError(t, err)
	defer n.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := n.DialContext(ctx, "tcp", "172.29.0.9:8080")
	require.Error(t, err)
	require.Nil(t, c)
}

func TestEthernetWriterBoundsBytesUnderLargeFrameBurst(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newEthernetConn(a, "", "")
	defer c.Close()
	frame := make([]byte, maxEthernetFrame+4)
	binary.BigEndian.PutUint32(frame, maxEthernetFrame)
	for range 1000 {
		n, err := c.Write(frame)
		require.NoError(t, err)
		require.Equal(t, len(frame), n)
	}
	require.LessOrEqual(t, c.queuedBytes.Load(), int64(ethernetQueueBytes))
	require.Less(t, len(c.writes), ethernetQueueFrames)
}
