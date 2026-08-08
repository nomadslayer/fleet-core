#!/usr/bin/env python3
"""Minimal Hrana-over-HTTP (/v2/pipeline) emulator backed by real SQLite.

Speaks enough of the sqld wire protocol to exercise TursoStore offline:
execute/close requests, typed args (text/integer/blob/null), typed row
values, affected_row_count. Not for production — testing only.

Usage: scripts/hrana_emulator.py [port] [db_path]
"""
import base64
import json
import sqlite3
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9090
DB = sys.argv[2] if len(sys.argv) > 2 else ":memory:"

conn = sqlite3.connect(DB, check_same_thread=False)
conn.execute("PRAGMA journal_mode=WAL")
lock = threading.Lock()


def decode_arg(a):
    t = a.get("type")
    if t == "text":
        return a["value"]
    if t == "integer":
        return int(a["value"])
    if t == "float":
        return float(a["value"])
    if t == "blob":
        return base64.b64decode(a["base64"])
    return None  # null


def encode_val(v):
    if v is None:
        return {"type": "null"}
    if isinstance(v, bytes):
        # Real sqld emits UNPADDED base64 for blobs. Padding here made the
        # emulator more forgiving than production and hid a decode bug that
        # broke every module fetch, so mirror sqld exactly.
        return {"type": "blob", "base64": base64.b64encode(v).decode().rstrip("=")}
    if isinstance(v, int):
        return {"type": "integer", "value": str(v)}
    if isinstance(v, float):
        return {"type": "float", "value": str(v)}
    return {"type": "text", "value": str(v)}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        if self.path != "/v2/pipeline":
            self.send_error(404)
            return
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        results = []
        for req in body.get("requests", []):
            if req.get("type") == "close":
                results.append({"type": "ok", "response": {"type": "close"}})
                continue
            stmt = req.get("stmt", {})
            args = [decode_arg(a) for a in stmt.get("args", [])]
            try:
                with lock:
                    cur = conn.execute(stmt.get("sql", ""), args)
                    rows = cur.fetchall()
                    conn.commit()
                cols = [{"name": d[0]} for d in (cur.description or [])]
                results.append({"type": "ok", "response": {"type": "execute", "result": {
                    "cols": cols,
                    "rows": [[encode_val(v) for v in row] for row in rows],
                    "affected_row_count": max(cur.rowcount, 0),
                }}})
            except Exception as e:  # surface as Hrana error result
                results.append({"type": "error", "error": {"message": str(e)}})
        out = json.dumps({"results": results}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)


if __name__ == "__main__":
    print(f"hrana emulator on 127.0.0.1:{PORT} db={DB}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
