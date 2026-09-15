#!/usr/bin/env python3
"""Run the host kernel HTTP/UDP tests against isolated loopback servers."""
import argparse
import re
import socketserver
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class HTTPHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        try:
            self.send_response(200)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"ok")
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *_args):
        pass

    def handle(self):
        try:
            super().handle()
        except (BrokenPipeError, ConnectionResetError):
            pass


class UDPHandler(socketserver.BaseRequestHandler):
    def handle(self):
        data, sock = self.request
        sock.sendto(data, self.client_address)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("bin_dir")
    args = parser.parse_args()
    servers = [ThreadingHTTPServer(("127.0.0.1", 0), HTTPHandler),
               socketserver.UDPServer(("127.0.0.1", 0), UDPHandler)]
    for server in servers:
        threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        target = "127.0.0.1:" + str(servers[0].server_address[1])
        for mode in ("poll", "select"):
            for connections, requests in ((5, 5), (24, 50)):
                result = subprocess.run([args.bin_dir + "/network_test", target, "-" + mode,
                                         "-connections", str(connections), "-requests", str(requests)],
                                        capture_output=True, text=True, timeout=60, check=True)
                counts = re.findall(r"c: (\d+) active: (\d+) req: (\d+) resp: (\d+)", result.stdout)
                assert counts, result.stdout
                count, active, _sent, responses = map(int, counts[-1])
                assert (count, active, responses) == (connections, 0, connections * requests), result.stdout
                print(f"PASS HTTP {mode}: {connections} connections, {responses} responses", flush=True)
        target = "127.0.0.1:" + str(servers[1].server_address[1])
        result = subprocess.run([args.bin_dir + "/udp_test", target, "-iterations", "200", "-localport", "0"],
                                capture_output=True, text=True, timeout=30, check=True)
        assert "success" in result.stdout, result.stdout + result.stderr
        print("PASS UDP: 200 iterations", flush=True)
    finally:
        for server in servers:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    main()
