#!/usr/bin/env python3
"""Mock GitHub API and smart HTTP Git server for M3 e2e tests.

Provides:
  - GitHub App installations and tokens (/app/installations, /app/installations/42/access_tokens)
  - Branch head lookup (/repos/{owner}/{repo}/branches/{branch})
  - Smart HTTP git fetch/clone for bare repos under $MOCK_REPOS_DIR
"""
import http.server
import json
import os
import subprocess
import sys
import urllib.parse

REPOS_DIR = os.environ.get("MOCK_REPOS_DIR", "/tmp/mock-repos")
PORT = int(os.environ.get("MOCK_GIT_PORT", "18090"))
GIT_EXEC = subprocess.check_output(["git", "--exec-path"], text=True).strip()
GIT_HTTP_BACKEND = os.path.join(GIT_EXEC, "git-http-backend")
HEAD_OVERRIDES = {}

class MockHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        if os.environ.get("MOCK_VERBOSE"):
            sys.stderr.write("%s - - [%s] %s\n" % (self.client_address[0], self.log_date_time_string(), format % args))

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path

        if path == "/app/installations":
            self.send_json(200, [{"id": 42, "account": {"login": "test-org"}}])
            return

        if path.startswith("/repos/") and "/branches/" in path:
            parts = path.split("/")
            if len(parts) >= 6:
                owner = parts[2]
                repo = parts[3]
                branch = parts[5]
                override_key = f"{owner}/{repo}:{branch}"
                if override_key in HEAD_OVERRIDES:
                    sha = HEAD_OVERRIDES[override_key]
                else:
                    repo_path = os.path.join(REPOS_DIR, owner, f"{repo}.git")
                    if not os.path.isdir(repo_path):
                        repo_path = os.path.join(REPOS_DIR, f"{repo}.git")
                    try:
                        sha = subprocess.check_output(
                            ["git", "--git-dir", repo_path, "rev-parse", branch],
                            text=True, stderr=subprocess.DEVNULL
                        ).strip()
                    except Exception:
                        sha = "0000000000000000000000000000000000000000"
                self.send_json(200, {"name": branch, "commit": {"sha": sha}})
                return

        if ".git" in path:
            self.run_git_http_backend("GET")
            return

        self.send_error(404, f"Not Found: {path}")

    def do_POST(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path

        if path == "/app/installations/42/access_tokens":
            self.send_json(201, {
                "token": "ghs_mock_token_abc123",
                "expires_at": "2099-01-01T00:00:00Z",
                "permissions": {"contents": "read"}
            })
            return

        if path == "/mock/set-head":
            length = int(self.headers.get("content-length", 0))
            body = json.loads(self.rfile.read(length))
            key = f"{body['owner']}/{body['repo']}:{body['branch']}"
            HEAD_OVERRIDES[key] = body["sha"]
            self.send_json(200, {"ok": True})
            return

        if ".git" in path:
            self.run_git_http_backend("POST")
            return

        self.send_error(404, f"Not Found: {path}")

    def do_DELETE(self):
        if self.path == "/installation/token":
            self.send_response(204)
            self.end_headers()
            return
        self.send_error(404, f"Not Found: {self.path}")

    def send_json(self, code, data):
        body = json.dumps(data).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def run_git_http_backend(self, method):
        parsed = urllib.parse.urlparse(self.path)
        env = os.environ.copy()
        env["GIT_PROJECT_ROOT"] = REPOS_DIR
        env["PATH_INFO"] = parsed.path
        env["QUERY_STRING"] = parsed.query or ""
        env["REQUEST_METHOD"] = method
        env["GIT_HTTP_EXPORT_ALL"] = "1"
        if "content-type" in self.headers:
            env["CONTENT_TYPE"] = self.headers["content-type"]
        if "content-length" in self.headers:
            env["CONTENT_LENGTH"] = self.headers["content-length"]

        input_data = None
        if method == "POST":
            length = int(self.headers.get("content-length", 0))
            input_data = self.rfile.read(length)

        p = subprocess.Popen(
            [GIT_HTTP_BACKEND],
            env=env,
            stdin=subprocess.PIPE if input_data else None,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE
        )
        out, err = p.communicate(input=input_data)
        if p.returncode != 0:
            self.send_error(500, f"git-http-backend failed: {err.decode('utf-8', 'ignore')}")
            return

        header_end = out.find(b"\r\n\r\n")
        delim_len = 4
        if header_end == -1:
            header_end = out.find(b"\n\n")
            delim_len = 2

        if header_end == -1:
            self.send_response(200)
            self.end_headers()
            self.wfile.write(out)
            return

        raw_headers = out[:header_end].decode("iso-8859-1")
        body = out[header_end + delim_len:]

        status_code = 200
        headers_to_send = []
        for line in raw_headers.splitlines():
            if not line:
                continue
            if line.lower().startswith("status:"):
                status_code = int(line.split()[1])
            else:
                parts = line.split(":", 1)
                if len(parts) == 2:
                    headers_to_send.append((parts[0].strip(), parts[1].strip()))

        self.send_response(status_code)
        for k, v in headers_to_send:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

if __name__ == "__main__":
    server = http.server.HTTPServer(("0.0.0.0", PORT), MockHandler)
    sys.stderr.write(f"Mock Git & GitHub server running on 0.0.0.0:{PORT}, repos in {REPOS_DIR}\n")
    sys.stderr.flush()
    server.serve_forever()
