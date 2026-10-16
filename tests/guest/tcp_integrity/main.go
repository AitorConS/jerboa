// Guest/host integrity fixture for concurrent TCP transfers and half-close.
// A host listener and concurrent guest clients compare independent SHA-256
// digests across mixed short writes, MSS tails and long streams.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

func fill(b []byte, stream uint64, offset uint64) {
	for i := range b {
		x := stream*0x9e3779b97f4a7c15 + (offset+uint64(i))*0xbf58476d1ce4e5b9
		b[i] = byte(x ^ (x >> 13) ^ (x >> 29))
	}
}

var initialPause = flag.Duration("initial-pause", 0, "pause before reading payload")
var readDelay = flag.Duration("read-delay", 0, "delay between payload reads")
var receiveBuffer = flag.Int("receive-buffer", 0, "socket receive buffer override")

func serveOne(c net.Conn) error {
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
		return fmt.Errorf("deadline: %w", err)
	}
	var meta [16]byte
	if _, err := io.ReadFull(c, meta[:]); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	stream := binary.BigEndian.Uint64(meta[:8])
	remaining := binary.BigEndian.Uint64(meta[8:])
	if *receiveBuffer > 0 {
		if t, ok := c.(*net.TCPConn); ok {
			if err := t.SetReadBuffer(*receiveBuffer); err != nil {
				return fmt.Errorf("receive buffer: %w", err)
			}
		}
	}
	time.Sleep(*initialPause)
	actual := sha256.New()
	expected := sha256.New()
	buf := make([]byte, 65536)
	want := make([]byte, len(buf))
	var offset uint64
	for remaining > 0 {
		n := uint64(len(buf))
		if remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(c, buf[:int(n)]); err != nil {
			return fmt.Errorf("stream %d: read payload: %w", stream, err)
		}
		fill(want[:int(n)], stream, offset)
		actual.Write(buf[:int(n)])
		expected.Write(want[:int(n)])
		offset += n
		remaining -= n
		time.Sleep(*readDelay)
	}
	if !equal(actual, expected) {
		return fmt.Errorf("stream %d: payload mismatch", stream)
	}
	digest := actual.Sum(nil)
	for written := 0; written < len(digest); {
		n, err := c.Write(digest[written:])
		if err != nil {
			return fmt.Errorf("stream %d: write digest: %w", stream, err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	// Keep the server VM alive until the peer confirms delivery of the digest.
	var ack [1]byte
	if _, err := io.ReadFull(c, ack[:]); err != nil {
		return fmt.Errorf("stream %d: read acknowledgement: %w", stream, err)
	}
	if ack[0] != 0xa5 {
		return fmt.Errorf("invalid completion acknowledgement")
	}
	var eof [1]byte
	if n, err := c.Read(eof[:]); n != 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("expected peer half-close: n=%d err=%w", n, err)
	}
	return nil
}

func equal(a, b hash.Hash) bool {
	x, y := a.Sum(nil), b.Sum(nil)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func client(addr string, stream, length uint64) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
		return fmt.Errorf("deadline: %w", err)
	}
	var meta [16]byte
	binary.BigEndian.PutUint64(meta[:8], stream)
	binary.BigEndian.PutUint64(meta[8:], length)
	for written := 0; written < len(meta); {
		n, err := c.Write(meta[written:])
		if err != nil {
			return fmt.Errorf("stream %d: write header: %w", stream, err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	chunks := []int{1, 73, 1420, 2840, 65535, 3, 1437, 8192}
	local := sha256.New()
	buf := make([]byte, 65535)
	var offset uint64
	for chunkIndex := 0; offset < length; chunkIndex++ {
		n := chunks[chunkIndex%len(chunks)]
		if uint64(n) > length-offset {
			n = int(length - offset)
		}
		fill(buf[:n], stream, offset)
		local.Write(buf[:n])
		for written := 0; written < n; {
			m, err := c.Write(buf[written:n])
			if err != nil {
				return fmt.Errorf("stream %d: write payload: %w", stream, err)
			}
			if m == 0 {
				return io.ErrShortWrite
			}
			written += m
		}
		offset += uint64(n)
	}
	var remote [sha256.Size]byte
	if _, err := io.ReadFull(c, remote[:]); err != nil {
		return fmt.Errorf("stream %d: read digest: %w", stream, err)
	}
	sum := local.Sum(nil)
	for i := range remote {
		if remote[i] != sum[i] {
			return fmt.Errorf("stream %d: digest mismatch", stream)
		}
	}
	if n, err := c.Write([]byte{0xa5}); err != nil || n != 1 {
		return fmt.Errorf("completion acknowledgement write: n=%d err=%w", n, err)
	}
	if t, ok := c.(*net.TCPConn); ok {
		if err := t.CloseWrite(); err != nil {
			return fmt.Errorf("stream %d: half-close: %w", stream, err)
		}
	}
	var eof [1]byte
	if n, err := c.Read(eof[:]); n != 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("expected server close: n=%d err=%w", n, err)
	}
	return nil
}

// listenAll serves workers connections on addr and reports whether every
// stream arrived intact.
func listenAll(addr string, workers int) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()
	if t, ok := ln.(*net.TCPListener); ok {
		if err := t.SetDeadline(time.Now().Add(120 * time.Second)); err != nil {
			return fmt.Errorf("listener deadline: %w", err)
		}
	}
	results := make(chan error, workers)
	for range workers {
		c, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go func() { results <- serveOne(c) }()
	}
	failed := false
	for range workers {
		if err := <-results; err != nil {
			fmt.Fprintln(os.Stderr, err)
			failed = true
		}
	}
	if failed {
		return errors.New("some streams failed")
	}
	return nil
}

func main() {
	if extra := os.Getenv("GSO_ARGS"); extra != "" {
		os.Args = append(os.Args, strings.Split(extra, ",")...)
	}
	listen := flag.Bool("listen", false, "run host listener")
	addr := flag.String("addr", "127.0.0.1:39123", "listen or connect address")
	workers := flag.Int("workers", 4, "concurrent TCP streams")
	length := flag.Uint64("bytes", 32<<20, "bytes per stream")
	flag.Parse()
	if *listen {
		if err := listenAll(*addr, *workers); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		time.Sleep(time.Second) // VM teardown grace after explicit peer digest acknowledgement.
		fmt.Println("host received all TCP payloads with matching SHA-256")
		return
	}
	var wg sync.WaitGroup
	errs := make(chan error, *workers)
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs <- client(*addr, uint64(i+1), *length) }(i)
	}
	wg.Wait()
	close(errs)
	failed := false
	for err := range errs {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
	fmt.Println("TCP payload SHA-256 checks passed")
}
