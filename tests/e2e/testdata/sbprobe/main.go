// sbprobe runs as a [build] run step of a project the end-to-end suite
// builds. It tries what a hostile build step would try, as told by
// sbprobe.json in the project directory, and records what worked in
// sbprobe-report.json. It always exits 0: the test judges the report.
package main

import (
	"encoding/json"
	"net"
	"os"
	"time"
)

type plan struct {
	// WriteFiles are created or overwritten with Content.
	WriteFiles []string `json:"write_files"`
	Content    string   `json:"content"`
	// ReadFiles are read.
	ReadFiles []string `json:"read_files"`
	// UnixSockets and TCPAddrs are connected to.
	UnixSockets []string `json:"unix_sockets"`
	TCPAddrs    []string `json:"tcp_addrs"`
}

// attempt is the outcome of one thing tried: Error is empty when it worked.
type attempt struct {
	Target string `json:"target"`
	Error  string `json:"error"`
	Data   string `json:"data,omitempty"`
}

type report struct {
	Writes  []attempt `json:"writes"`
	Reads   []attempt `json:"reads"`
	Sockets []attempt `json:"sockets"`
	TCP     []attempt `json:"tcp"`
	TempDir attempt   `json:"temp_dir"`
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func main() {
	var p plan
	data, err := os.ReadFile("sbprobe.json")
	if err == nil {
		err = json.Unmarshal(data, &p)
	}
	if err != nil {
		_ = os.WriteFile("sbprobe-report.json", []byte(`{"fatal":"`+err.Error()+`"}`), 0o600)
		return
	}
	var r report
	for _, f := range p.WriteFiles {
		r.Writes = append(r.Writes, attempt{Target: f, Error: errString(os.WriteFile(f, []byte(p.Content), 0o600))})
	}
	for _, f := range p.ReadFiles {
		b, err := os.ReadFile(f)
		r.Reads = append(r.Reads, attempt{Target: f, Error: errString(err), Data: string(b)})
	}
	for _, s := range p.UnixSockets {
		conn, err := net.DialTimeout("unix", s, 3*time.Second)
		if err == nil {
			_ = conn.Close()
		}
		r.Sockets = append(r.Sockets, attempt{Target: s, Error: errString(err)})
	}
	for _, a := range p.TCPAddrs {
		conn, err := net.DialTimeout("tcp", a, 3*time.Second)
		if err == nil {
			_ = conn.Close()
		}
		r.TCP = append(r.TCP, attempt{Target: a, Error: errString(err)})
	}
	// The step's own temporary directory must work: tools need one.
	f, err := os.CreateTemp("", "sbprobe-*")
	r.TempDir = attempt{Target: os.TempDir(), Error: errString(err)}
	if err == nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	out, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile("sbprobe-report.json", out, 0o600)
}
