"""Minimal fake GitLab API for testing the webhook registration of the ansibled role.

Serves just what ansible.builtin.uri and community.general.gitlab_hook (python-gitlab)
call: GET /user, GET /projects/:id, GET|POST /projects/:id/hooks,
PUT /projects/:id/hooks/:hook_id. GET /_log returns the recorded writes.

Usage: python3 fake_gitlab.py PORT TOKEN
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote, urlsplit

PROJECT = {"id": 7, "path_with_namespace": "myorg/infra", "name": "infra"}
HOOKS = {}
LOG = []


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def route(self, method):
        path = urlsplit(self.path).path
        if path == "/_log":
            return self.reply(200, LOG)
        if self.headers.get("PRIVATE-TOKEN") != TOKEN:
            return self.reply(401, {"message": "401 Unauthorized"})
        if not path.startswith("/api/v4/"):
            return self.reply(404, {"message": "404 Not Found"})
        parts = path[len("/api/v4/"):].split("/")
        if parts == ["user"]:
            return self.reply(200, {"id": 1, "username": "bot"})
        if len(parts) < 2 or parts[0] != "projects" or unquote(parts[1]) not in ("7", "myorg/infra"):
            return self.reply(404, {"message": "404 Project Not Found"})
        rest = parts[2:]
        if rest == [] and method == "GET":
            return self.reply(200, PROJECT)
        if rest == ["hooks"] and method == "GET":
            return self.reply(200, list(HOOKS.values()))
        body = {}
        if method in ("POST", "PUT"):
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n) or b"{}")
        if rest == ["hooks"] and method == "POST":
            hid = len(HOOKS) + 1
            hook = {k: v for k, v in body.items() if k != "token"}
            hook.update(id=hid, project_id=7)
            HOOKS[hid] = hook
            LOG.append({"op": "create", "id": hid, "body": body})
            return self.reply(201, hook)
        if len(rest) == 2 and rest[0] == "hooks" and method == "PUT":
            hid = int(rest[1])
            if hid not in HOOKS:
                return self.reply(404, {"message": "404 Not Found"})
            HOOKS[hid].update({k: v for k, v in body.items() if k != "token"})
            LOG.append({"op": "update", "id": hid, "body": body})
            return self.reply(200, HOOKS[hid])
        return self.reply(404, {"message": "404 Not Found"})

    def do_GET(self):
        self.route("GET")

    def do_POST(self):
        self.route("POST")

    def do_PUT(self):
        self.route("PUT")


if __name__ == "__main__":
    TOKEN = sys.argv[2]
    ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
