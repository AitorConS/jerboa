// voltest — HTTP server that persists messages to a mounted volume.
//
// Endpoints:
//
//	POST /write?msg=hello   append a message to /data/log.txt
//	GET  /                  list all messages written so far
//	DELETE /reset           clear the log
//
// Mount a volume at /data to verify persistence across VM restarts:
//
//	jerboa volume create testdata
//	jerboa build ./voltest --name voltest
//	jerboa run voltest:latest -v testdata:/data -p 8080:8080 --name v1
//	curl -X POST "http://localhost:8080/write?msg=hello"
//	curl -X POST "http://localhost:8080/write?msg=world"
//	curl http://localhost:8080/
//	jerboa stop v1 && jerboa rm v1
//	jerboa run voltest:latest -v testdata:/data -p 8080:8080 --name v2
//	curl http://localhost:8080/   ← still shows "hello" and "world"
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

const logFile = "/data/log.txt"

func main() {
	// Ensure the data directory exists — the volume mount point may not be
	// pre-created inside the guest filesystem.
	if err := os.MkdirAll("/data", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create /data: %v\n", err)
		os.Exit(1)
	}

	http.HandleFunc("/", handleList)
	http.HandleFunc("/write", handleWrite)
	http.HandleFunc("/reset", handleReset)

	log.Println("voltest listening on :8080")
	log.Printf("log file: %s", logFile)
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

func handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	data, err := os.ReadFile(logFile)
	if os.IsNotExist(err) {
		fmt.Fprintln(w, "(no messages yet — POST /write?msg=hello to add one)")
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	if _, err := w.Write(data); err != nil {
		http.Error(w, fmt.Sprintf("write error: %v", err), http.StatusInternalServerError)
	}
}

func handleWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	msg := r.URL.Query().Get("msg")
	if msg == "" {
		http.Error(w, "missing ?msg=", http.StatusBadRequest)
		return
	}
	line := fmt.Sprintf("[%s] %s\n", time.Now().UTC().Format(time.RFC3339), msg)
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		http.Error(w, fmt.Sprintf("open error: %v", err), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		http.Error(w, fmt.Sprintf("write error: %v", err), http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "written: %s", line)
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "use DELETE", http.StatusMethodNotAllowed)
		return
	}
	if err := os.Remove(logFile); err != nil && !os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("remove error: %v", err), http.StatusInternalServerError)
		return
	}
	fmt.Fprintln(w, "log cleared")
}
