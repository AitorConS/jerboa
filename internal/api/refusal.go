package api

import (
	"encoding/json"
	"net"
	"time"
)

// refusalWait bounds how long a client whose stream was cut waits for the
// daemon's reason.
const refusalWait = 2 * time.Second

// refusal explains a stream the daemon stopped reading. A daemon that
// refuses a streamed request (an unknown method, a name already taken)
// answers and closes without reading the rest, so the client's next write
// fails with a broken pipe while the reason sits unread on the connection.
// refusal returns that reason when there is one, and streamErr otherwise.
func refusal(conn net.Conn, dec *json.Decoder, streamErr error) error {
	if err := conn.SetReadDeadline(time.Now().Add(refusalWait)); err != nil {
		return streamErr
	}
	var resp Response
	if err := dec.Decode(&resp); err != nil || resp.Error == nil {
		return streamErr
	}
	return resp.Error
}
