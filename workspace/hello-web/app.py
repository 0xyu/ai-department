#!/usr/bin/env python3
"""Hello World web server (stdlib only).

Usage: python app.py   (listens on 0.0.0.0, port from env PORT, default 8000)
"""
import os
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PAGE = ("<!DOCTYPE html><html><head><meta charset=\"utf-8\">"
        "<title>Hello</title></head><body><h1>Hello, World!</h1></body></html>").encode()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.split("?", 1)[0] == "/":
            self._send(200, PAGE, "text/html; charset=utf-8")
        else:
            self._send(404, b"404 Not Found", "text/plain; charset=utf-8")

    def _send(self, code, body, ctype):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def make_server(port=None, host="0.0.0.0"):
    if port is None:
        port = int(os.environ.get("PORT", "8000"))
    return ThreadingHTTPServer((host, port), Handler)


def main():
    argparse.ArgumentParser(description="Hello World web server; set port via PORT env (default 8000).").parse_args()
    srv = make_server()
    print("Serving on port %d" % srv.server_address[1])
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
