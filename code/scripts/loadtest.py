#!/usr/bin/env python3
"""Concurrent sandbox startup load test (step 5.5, D48). Stdlib only.

Creates N sandboxes at the same moment through the API and, for each, times:
  create   - the POST /v1/sandboxes round trip
  running  - from the POST until the API reports status "running": the pod
             is scheduled, started under gVisor, and its readiness probe (a
             TCP check of sshd's port, every second) passed and was synced
  exec     - from the POST until a first `echo` through the API succeeds,
             which includes the API dialling the sandbox over SSH
Prints p50/p95/max (nearest rank) for each and writes every row to a CSV.
Every sandbox created is deleted at the end, also on errors and Ctrl-C.

Usage: loadtest.py --api http://127.0.0.1:18094/v1 --key-file FILE
                   --pubkey-file FILE.pub --n 10 --csv OUT
"""
import argparse
import csv
import json
import math
import os
import signal
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request


def call(api, key, method, path, body=None, timeout=60):
    """One API call; never raises: transport errors come back as code 0."""
    req = urllib.request.Request(api + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": "Bearer " + key})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            code, raw = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, raw = e.code, e.read()
    except Exception as e:  # URLError, timeouts, connection resets
        return 0, {"error": f"{type(e).__name__}: {e}"}
    try:
        return code, json.loads(raw or b"{}")
    except ValueError:
        return code, {"error": raw[:200].decode(errors="replace")}


class Created:
    """Sandbox ids created so far, recorded the moment the API returns them,
    so cleanup can find them whatever happens to the measuring thread."""

    def __init__(self):
        self.lock, self.ids = threading.Lock(), []

    def add(self, sid):
        with self.lock:
            self.ids.append(sid)


def one(api, key, pubkey, image, deadline_s, start, created, out, i):
    row = {"i": i, "id": "", "create_s": None, "running_s": None, "exec_s": None, "error": ""}
    out[i] = row
    try:
        start.wait()
        t0 = time.monotonic()
        req = {"image": image, "ssh_public_key": pubkey}
        if os.environ.get("SANDBOX_RUNTIME"):  # e.g. kata (D65)
            req["runtime"] = os.environ["SANDBOX_RUNTIME"]
        code, body = call(api, key, "POST", "/sandboxes", req)
        row["create_s"] = time.monotonic() - t0
        if code != 201:
            row["error"] = f"create {code}: {body.get('error', body)}"
            return
        sid = row["id"] = body["id"]
        created.add(sid)
        while True:
            if time.monotonic() - t0 > deadline_s:
                row["error"] = f"not running after {deadline_s}s (status {body.get('status')})"
                return
            code, body = call(api, key, "GET", "/sandboxes/" + sid)
            if code == 200 and body.get("status") == "running":
                row["running_s"] = time.monotonic() - t0
                break
            if code == 200 and body.get("status") in ("failed", "lost", "deleted"):
                row["error"] = "ended: " + (body.get("status_reason") or body["status"])
                return
            time.sleep(0.25)
        while True:
            if time.monotonic() - t0 > deadline_s:
                row["error"] = "exec never succeeded"
                return
            code, body = call(api, key, "POST", f"/sandboxes/{sid}/exec", {"command": "echo ok"})
            if code == 200 and body.get("stdout", "").strip() == "ok":
                row["exec_s"] = time.monotonic() - t0
                return
            time.sleep(0.25)
    except Exception as e:  # anything unexpected: record it, never lose the row
        row["error"] = f"{type(e).__name__}: {e}"


def pct(values, p):
    """Nearest-rank percentile: the smallest value with at least p% of the
    values at or below it."""
    if not values:
        return None
    s = sorted(values)
    return s[max(0, math.ceil(p / 100 * len(s)) - 1)]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", required=True)
    ap.add_argument("--key-file", required=True)
    ap.add_argument("--pubkey-file", required=True)
    ap.add_argument("--n", type=int, default=10)
    ap.add_argument("--image", default="ubuntu-base")
    ap.add_argument("--deadline", type=float, default=180)
    ap.add_argument("--csv", required=True)
    a = ap.parse_args()
    key = open(a.key_file).read().strip()
    pubkey = open(a.pubkey_file).read().strip()

    # Ctrl-C and SIGTERM (terminal closed, kill) both end in the cleanup
    # below. Set explicitly: a shell may start background jobs with SIGINT
    # ignored, and Python's default SIGTERM skips `finally`.
    def stop(signum, _frame):
        raise KeyboardInterrupt(signal.Signals(signum).name)
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)

    out = [None] * a.n
    created = Created()
    start = threading.Event()
    threads = [threading.Thread(target=one, daemon=True,
                                args=(a.api, key, pubkey, a.image, a.deadline, start, created, out, i))
               for i in range(a.n)]
    wall = 0.0
    try:
        for t in threads:
            t.start()
        wall0 = time.monotonic()
        start.set()  # all creates fire together
        for t in threads:
            while t.is_alive():  # join with a timeout, so signals are handled promptly
                t.join(0.2)
        wall = time.monotonic() - wall0
        with open(a.csv, "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=["i", "id", "create_s", "running_s", "exec_s", "error"])
            w.writeheader()
            w.writerows(r for r in out if r)
    finally:  # also on Ctrl-C: delete everything this run created
        with created.lock:
            ids = set(created.ids)
        # Sweep too: a create still in flight when the run was interrupted
        # (or that timed out on our side) has no recorded id. The key
        # belongs to the dedicated load-test user, so every live sandbox
        # it can see is ours to delete (Phase 5 batch review).
        code, body = call(a.api, key, "GET", "/sandboxes")
        if code == 200:
            ids |= {s["id"] for s in body.get("sandboxes", [])}
        for sid in ids:
            call(a.api, key, "DELETE", "/sandboxes/" + sid)
        if ids:
            print(f"cleaned up {len(ids)} sandboxes", file=sys.stderr)

    ok = [r for r in out if r and r["exec_s"] is not None]
    print(f"{a.n} sandboxes ({a.image}) created at once; {len(ok)} reached running + exec; wall {wall:.1f}s")
    for field in ("create_s", "running_s", "exec_s"):
        vals = [r[field] for r in out if r and r[field] is not None]
        if vals:
            print(f"  {field[:-2]:8} n={len(vals):3}  p50={pct(vals, 50):6.2f}s  p95={pct(vals, 95):6.2f}s"
                  f"  max={max(vals):6.2f}s  mean={statistics.mean(vals):6.2f}s")
    errors = [r for r in out if not r or r["error"]]
    for r in errors[:5]:
        print(f"  error #{r['i'] if r else '?'}: {r['error'] if r else 'no result'}")
    if len(errors) > 5:
        print(f"  … {len(errors) - 5} more errors (see {a.csv})")
    return 0 if not errors else 1


if __name__ == "__main__":
    sys.exit(main())
