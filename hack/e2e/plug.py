#!/usr/bin/env python3
"""A fake smart plug for the k3s end-to-end test.

nodenap's http driver calls it like a Shelly or Tasmota plug; it "switches
power" by stopping and starting the k3d node container instead:

  GET /on?node=NAME    docker start NAME
  GET /off?node=NAME   docker stop NAME   (a clean shutdown: SIGTERM, then kill)
  GET /status?node=NAME  {"on": bool, "apower": watts}

Only containers whose name starts with PREFIX can be switched. Every call is
appended to LOG, one line each, for the test to check.

Usage: plug.py PORT PREFIX LOG
"""

import json
import subprocess
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

PORT, PREFIX, LOG = int(sys.argv[1]), sys.argv[2], sys.argv[3]


def running(node):
    out = subprocess.run(["docker", "inspect", "-f", "{{.State.Running}}", node],
                         capture_output=True, text=True)
    return out.stdout.strip() == "true"


class Plug(BaseHTTPRequestHandler):
    def do_GET(self):
        url = urlparse(self.path)
        node = parse_qs(url.query).get("node", [""])[0]
        if not node.startswith(PREFIX):
            return self.reply(400, {"error": f"node must start with {PREFIX}"})
        action = url.path.strip("/")
        if action in ("on", "off"):
            with open(LOG, "a") as f:
                f.write(f"{time.strftime('%H:%M:%S')} {action} {node}\n")
            cmd = ["docker", "start", node] if action == "on" else ["docker", "stop", "-t", "20", node]
            # Like a plug, answer at once; the machine changes state afterwards.
            subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            return self.reply(200, {"ok": True})
        if action == "status":
            on = running(node)
            return self.reply(200, {"on": on, "apower": 20.0 if on else 0.5})
        return self.reply(404, {"error": "unknown path"})

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        sys.stderr.write("plug: " + fmt % args + "\n")


ThreadingHTTPServer(("0.0.0.0", PORT), Plug).serve_forever()
