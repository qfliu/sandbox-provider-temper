#!/usr/bin/env python3
"""Concurrent sandbox startup load test (step 5.5, D48). Stdlib only.

Creates N sandboxes at the same moment through the API and, for each, times:
  create   - the POST /v1/sandboxes round trip
  running  - from the POST until the API reports status "running"
             (the pod is scheduled, started under gVisor and sshd answers)
  exec     - from the POST until a first `echo` through the API succeeds
             (the API's own SSH connection into the sandbox works)
Prints p50/p95/max for each and writes every row to a CSV. Every sandbox
created is deleted at the end, whatever happened.

Usage: loadtest.py --api http://127.0.0.1:18094/v1 --key-file FILE --pubkey-file FILE.pub --n 10 --csv OUT
"""
import argparse
import csv
import json
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request

def call(api, key, method, path, body=None, timeout=60):
    req = urllib.request.Request(api + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": "Bearer " + key})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def one(api, key, pubkey, image, deadline_s, start, out, i):
    row = {"i": i, "id": "", "create_s": None, "running_s": None, "exec_s": None, "error": ""}
    start.wait()
    t0 = time.monotonic()
    code, body = call(api, key, "POST", "/sandboxes", {"image": image, "ssh_public_key": pubkey})
    row["create_s"] = time.monotonic() - t0
    if code != 201:
        row["error"] = f"create {code}: {body.get('error', body)}"
        out[i] = row
        return
    sid = row["id"] = body["id"]
    while time.monotonic() - t0 < deadline_s:
        code, body = call(api, key, "GET", "/sandboxes/" + sid)
        if code == 200 and body.get("status") == "running":
            row["running_s"] = time.monotonic() - t0
            break
        if code == 200 and body.get("status") in ("failed", "lost", "deleted"):
            row["error"] = "ended: " + body.get("status_reason", body["status"])
            out[i] = row
            return
        time.sleep(0.25)
    else:
        row["error"] = f"not running after {deadline_s}s (status {body.get('status')})"
        out[i] = row
        return
    while time.monotonic() - t0 < deadline_s:
        code, body = call(api, key, "POST", f"/sandboxes/{sid}/exec", {"command": "echo ok"})
        if code == 200 and body.get("stdout", "").strip() == "ok":
            row["exec_s"] = time.monotonic() - t0
            break
        time.sleep(0.25)
    else:
        row["error"] = "exec never succeeded"
    out[i] = row


def pct(values, p):
    if not values:
        return None
    s = sorted(values)
    k = max(0, min(len(s) - 1, round(p / 100 * len(s) + 0.5) - 1))  # nearest-rank
    return s[k]


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

    out = [None] * a.n
    start = threading.Event()
    threads = [threading.Thread(target=one, args=(a.api, key, pubkey, a.image, a.deadline, start, out, i)) for i in range(a.n)]
    for t in threads:
        t.start()
    wall0 = time.monotonic()
    start.set()  # all creates fire together
    for t in threads:
        t.join()
    wall = time.monotonic() - wall0

    try:
        with open(a.csv, "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=["i", "id", "create_s", "running_s", "exec_s", "error"])
            w.writeheader()
            w.writerows(out)
    finally:
        for r in out:  # clean up everything we created
            if r and r["id"]:
                call(a.api, key, "DELETE", "/sandboxes/" + r["id"])

    ok = [r for r in out if r and r["exec_s"] is not None]
    print(f"{a.n} sandboxes ({a.image}) created at once; {len(ok)} reached running + exec; wall {wall:.1f}s")
    for field in ("create_s", "running_s", "exec_s"):
        vals = [r[field] for r in out if r and r[field] is not None]
        if vals:
            print(f"  {field[:-2]:8} n={len(vals):3}  p50={pct(vals, 50):6.2f}s  p95={pct(vals, 95):6.2f}s"
                  f"  max={max(vals):6.2f}s  mean={statistics.mean(vals):6.2f}s")
    errors = [r for r in out if r and r["error"]]
    for r in errors[:5]:
        print(f"  error #{r['i']}: {r['error']}")
    if len(errors) > 5:
        print(f"  … {len(errors) - 5} more errors (see {a.csv})")
    return 0 if not errors else 1


if __name__ == "__main__":
    sys.exit(main())
