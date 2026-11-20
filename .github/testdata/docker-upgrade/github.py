import json
import ssl
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class GitHub(BaseHTTPRequestHandler):
    def respond(self, body):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        if self.path == "/users/upgrade-fixture/installation":
            self.respond({"id": 1, "account": {"login": "upgrade-fixture", "type": "User"}})
        elif self.path == "/repos/upgrade-fixture/private-repo":
            self.respond({"id": 2, "private": True, "owner": {"login": "upgrade-fixture"}})
        elif self.path == "/repos/upgrade-fixture/private-repo/actions/jobs/7001":
            self.respond({"status": "queued"})
        else:
            self.send_error(404)

    def do_POST(self):
        if self.path == "/app/installations/1/access_tokens":
            expiry = datetime.now(timezone.utc) + timedelta(hours=1)
            self.respond({"token": "fixture-token", "expires_at": expiry.isoformat()})
        elif self.path == "/repos/upgrade-fixture/private-repo/actions/runners/generate-jitconfig":
            self.respond({"runner": {"id": 42}, "encoded_jit_config": "upgrade-fixture-jit"})
        else:
            self.send_error(404)

    def do_DELETE(self):
        if self.path == "/repos/upgrade-fixture/private-repo/actions/runners/42":
            self.send_response(204)
            self.end_headers()
        else:
            self.send_error(404)


server = ThreadingHTTPServer(("0.0.0.0", 443), GitHub)
tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
tls.load_cert_chain("/fixture/github.pem", "/fixture/github.key")
server.socket = tls.wrap_socket(server.socket, server_side=True)
server.serve_forever()
