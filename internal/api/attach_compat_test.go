package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
)

// attachFromWire exercises the public client against a peer that sends the
// supplied wire bytes. With no cuts, the response and stream use one write.
func attachFromWire(t *testing.T, wire []byte, cuts []int, out io.Writer) error {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			serverDone <- err
			return
		}
		var req api.Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			serverDone <- fmt.Errorf("read attach request: %w", err)
			return
		}
		var params api.AttachParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			serverDone <- fmt.Errorf("decode attach params: %w", err)
			return
		}
		if req.Method != "VM.Attach" || params.ID != "vm-test" || !params.Framed {
			serverDone <- fmt.Errorf("unexpected attach request: method=%q params=%+v", req.Method, params)
			return
		}
		if len(cuts) == 0 {
			_, err = conn.Write(wire)
		} else {
			pos := 0
			for _, size := range cuts {
				if pos >= len(wire) {
					break
				}
				end := min(pos+size, len(wire))
				if _, err = conn.Write(wire[pos:end]); err != nil {
					break
				}
				pos = end
			}
			if err == nil && pos < len(wire) {
				_, err = conn.Write(wire[pos:])
			}
		}
		serverDone <- err
	}()

	client, err := api.DialWithToken("tcp://"+listener.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	attachDone := make(chan error, 1)
	go func() { attachDone <- client.Attach(context.Background(), "vm-test", out) }()

	var attachErr error
	select {
	case attachErr = <-attachDone:
	case <-time.After(4 * time.Second):
		_ = client.Close()
		t.Fatal("Attach did not finish after peer closed")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("attach peer: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("attach peer did not finish")
	}
	return attachErr
}

func attachResponse(t *testing.T, result json.RawMessage) []byte {
	t.Helper()
	var wire bytes.Buffer
	if err := json.NewEncoder(&wire).Encode(api.Response{JSONRPC: "2.0", ID: 1, Result: result}); err != nil {
		t.Fatal(err)
	}
	return wire.Bytes()
}

func framedAttachWire(t *testing.T, payloads [][]byte, terminal *api.AttachResult, closeFrame bool) []byte {
	t.Helper()
	var wire bytes.Buffer
	wire.Write(attachResponse(t, json.RawMessage(`{"framed":true}`)))
	frames := api.NewFrameWriter(&wire)
	for _, payload := range payloads {
		if _, err := frames.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if closeFrame {
		if err := frames.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if terminal != nil {
		if err := json.NewEncoder(&wire).Encode(terminal); err != nil {
			t.Fatal(err)
		}
	}
	return wire.Bytes()
}

func TestAttachLegacySameWritePreservesRawConsole(t *testing.T) {
	console := []byte("legacy console\n\x00\xff{\"done\":true}\n")
	wire := append(attachResponse(t, json.RawMessage(`{}`)), console...)
	var got bytes.Buffer
	if err := attachFromWire(t, wire, nil, &got); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !bytes.Equal(got.Bytes(), console) {
		t.Fatalf("console = %q, want %q", got.Bytes(), console)
	}
}

func TestAttachFramedFragmentedBinaryConsole(t *testing.T) {
	console := []byte{0, 255, 1, '\n', '{', '"', 'd', 'o', 'n', 'e', '"', ':', 't', 'r', 'u', 'e', '}', '\n', 0, 0, 0, 0}
	wire := framedAttachWire(t, [][]byte{console[:7], console[7:]}, &api.AttachResult{Done: true}, true)
	var got bytes.Buffer
	// Repeated tiny writes split the JSON response, frame headers, payload,
	// terminator, and terminal JSON at different boundaries.
	var cuts []int
	for n := 0; n < len(wire); n++ {
		cuts = append(cuts, 1+n%3)
	}
	if err := attachFromWire(t, wire, cuts, &got); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !bytes.Equal(got.Bytes(), console) {
		t.Fatalf("console = %q, want %q", got.Bytes(), console)
	}
}

func TestAttachFramedInvalidEndings(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
	}{
		{name: "missing terminal", wire: framedAttachWire(t, [][]byte{[]byte("output")}, nil, true)},
		{name: "done false", wire: framedAttachWire(t, [][]byte{[]byte("output")}, &api.AttachResult{Done: false}, true)},
		{name: "truncated frame", wire: framedAttachWire(t, [][]byte{[]byte("output")}, nil, false)[:len(attachResponse(t, json.RawMessage(`{"framed":true}`)))+6]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bytes.Buffer
			if err := attachFromWire(t, tc.wire, nil, &got); err == nil {
				t.Fatal("Attach succeeded without a complete terminal status")
			}
		})
	}
}

type attachFailWriter struct{ err error }

func (w attachFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestAttachFramedOutputWriterError(t *testing.T) {
	want := errors.New("output writer failed")
	wire := framedAttachWire(t, [][]byte{[]byte("output")}, &api.AttachResult{Done: true}, true)
	if err := attachFromWire(t, wire, nil, attachFailWriter{err: want}); !errors.Is(err, want) {
		t.Fatalf("Attach error = %v, want output writer error", err)
	}
}
