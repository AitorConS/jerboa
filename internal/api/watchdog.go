package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// A call has no deadline of its own: a build or a pull may take as long as
// it takes. What a call must not do is wait forever on a daemon that is no
// longer there to answer (stuck, or stopped under a debugger) while its
// socket still accepts connections. The watchdog tells the two apart
// without knowing how long any call should take: once a call has waited
// watchdogDelay it asks the daemon, on a connection of its own, a question
// every live daemon answers at once. Silence ends the call; an answer means
// the daemon is busy, and the call keeps waiting.
var (
	watchdogDelay    = 10 * time.Second
	watchdogInterval = 30 * time.Second
)

// ErrDaemonUnresponsive is returned by a call when the daemon stopped
// answering, as opposed to taking long over the call.
var ErrDaemonUnresponsive = errors.New("the daemon is not answering; it may be stuck (try `jerboa daemon restart`)")

// watchReply watches over one call that is waiting for its answer. The
// returned function ends the watch and reports ErrDaemonUnresponsive when
// the watchdog gave up on the daemon, in which case the connection has been
// given a deadline in the past to release the call.
func (c *Client) watchReply() func() error {
	done := make(chan struct{})
	finished := make(chan struct{})
	var stuck error
	go func() {
		defer close(finished)
		t := time.NewTimer(watchdogDelay)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			if !c.answers(done) {
				stuck = ErrDaemonUnresponsive
				_ = c.conn.SetDeadline(deadlinePast)
				return
			}
			t.Reset(watchdogInterval)
		}
	}()
	return func() error {
		close(done)
		<-finished
		return stuck
	}
}

// answers reports whether the daemon answers a request on a fresh
// connection within the handshake deadline. Only silence counts as no: a
// refused connection, an error reply or a failed authentication are left
// for the call itself to run into. It returns early, with yes, when done
// closes.
func (c *Client) answers(done <-chan struct{}) bool {
	conn, err := dialDaemon(dialArgs(c.endpoint))
	if err != nil {
		return !isTimeout(err)
	}
	probed := make(chan struct{})
	defer close(probed)
	go func() {
		select {
		case <-done:
		case <-probed:
		}
		_ = conn.Close()
	}()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	if c.token != "" {
		err = authenticate(conn, enc, dec, c.token)
	} else {
		err = ask(conn, enc, dec)
	}
	select {
	case <-done:
		return true
	default:
	}
	return err == nil || !isTimeout(err)
}

// ask sends the request every daemon answers and waits for any reply.
func ask(conn net.Conn, enc *json.Encoder, dec *json.Decoder) error {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return fmt.Errorf("set probe deadline: %w", err)
	}
	if err := enc.Encode(Request{JSONRPC: "2.0", ID: 1, Method: "Daemon.Version"}); err != nil {
		return fmt.Errorf("send probe: %w", err)
	}
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("read probe reply: %w", err)
	}
	return nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
