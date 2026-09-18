// Command testapp is the HTTP service used by jerboa-bench. The same static
// binary is packaged for every runtime (Jerboa image and FROM scratch Docker
// image) so the comparison measures the runtime, not the application.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	started := time.Now()
	payload := []byte(`{"service":"jerboa-bench","ok":true}` + "\n")

	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok %d\n", time.Since(started).Microseconds())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// /?bytes=N returns an N-byte body to exercise larger responses.
		if n, err := strconv.Atoi(r.URL.Query().Get("bytes")); err == nil && n > 0 && n <= 64<<20 {
			w.Header().Set("Content-Length", strconv.Itoa(n))
			buf := make([]byte, 32<<10)
			for n > 0 {
				chunk := min(n, len(buf))
				if _, err := w.Write(buf[:chunk]); err != nil {
					return
				}
				n -= chunk
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	})
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	// Logging the parsed number keeps unvalidated environment input out of the log.
	listenPort, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("invalid PORT: %v", err)
	}
	log.Printf("jerboa-bench testapp listening on :%d", listenPort)
	log.Fatal(srv.ListenAndServe())
}
