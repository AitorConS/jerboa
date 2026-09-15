//go:build darwin && arm64

// snapshot-switch is a deliberately small acceptance harness for the
// Firecracker unix-stream transport. It is not daemon implementation code.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/AitorConS/jerboa/internal/network"
)

func answer(query []byte, source string) ([]byte, error) {
	if len(query) < 17 {
		return nil, fmt.Errorf("short DNS query")
	}
	end := 12
	var labels []byte
	for end < len(query) && query[end] != 0 {
		size := int(query[end])
		if size == 0 || end+1+size >= len(query) {
			return nil, fmt.Errorf("bad DNS name")
		}
		if len(labels) != 0 {
			labels = append(labels, '.')
		}
		labels = append(labels, query[end+1:end+1+size]...)
		end += 1 + size
	}
	end++
	if end+4 > len(query) {
		return nil, fmt.Errorf("short DNS question")
	}
	response := append([]byte(nil), query[:end+4]...)
	response[2], response[3] = 0x81, 0x83
	response[6], response[7] = 0, 0
	if source == "172.25.40.11" && string(labels) == "peer" && binary.BigEndian.Uint16(query[end:end+2]) == 1 {
		response[3], response[6], response[7] = 0x80, 0, 1
		response = append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 30, 0, 4, 172, 25, 40, 11)
	}
	return response, nil
}

func main() {
	if len(os.Args) != 6 {
		panic("usage: snapshot-switch SOCKET MAC HTTP UDP TCP")
	}
	httpPort, err := strconv.Atoi(os.Args[3])
	if err != nil {
		panic(err)
	}
	udpPort, err := strconv.Atoi(os.Args[4])
	if err != nil {
		panic(err)
	}
	tcpPort, err := strconv.Atoi(os.Args[5])
	if err != nil {
		panic(err)
	}
	n, err := network.NewNativeNetworkWithPolicy("172.25.40.0/24", "172.25.40.1", answer,
		func(string, string, uint16, bool) bool { return false })
	if err != nil {
		panic(err)
	}
	defer n.Close()
	link, err := n.ListenVM(os.Args[1], "172.25.40.11", os.Args[2])
	if err != nil {
		panic(err)
	}
	defer link.Close()
	for _, publish := range []struct {
		protocol string
		port     int
		guest    string
	}{
		{"tcp", httpPort, "172.25.40.11:8080"}, {"udp", udpPort, "172.25.40.11:8081"}, {"tcp", tcpPort, "172.25.40.11:8082"},
	} {
		if err := n.Expose(publish.protocol, fmt.Sprintf("127.0.0.1:%d", publish.port), publish.guest); err != nil {
			panic(err)
		}
	}
	fmt.Println("READY")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
}
