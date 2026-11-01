#!/usr/bin/env python3
"""Count lines of code per file type under a directory.

Usage: count_lines.py [root] [--json]
Skips .git, node_modules, __pycache__, .venv, etc. and binary files.
"""
import argparse
import json
import os
from collections import defaultdict

SKIP_DIRS = {".git", "node_modules", "__pycache__", ".venv", "venv", ".idea", ".pytest_cache"}


def count(root):
    stats = defaultdict(lambda: {"files": 0, "total": 0, "code": 0, "blank": 0})
    for dp, dns, fns in os.walk(root):
        dns[:] = [d for d in dns if d not in SKIP_DIRS]
        for fn in fns:
            path = os.path.join(dp, fn)
            ext = os.path.splitext(fn)[1].lower() or "(none)"
            try:
                with open(path, "rb") as f:
                    data = f.read()
            except OSError:
                continue
            if b"\0" in data:
                continue
            lines = data.decode("utf-8", "replace").splitlines()
            blank = sum(1 for l in lines if not l.strip())
            s = stats[ext]
            s["files"] += 1
            s["total"] += len(lines)
            s["blank"] += blank
            s["code"] += len(lines) - blank
    return dict(stats)


def main():
    ap = argparse.ArgumentParser(description="Count lines per file type, skipping .git etc.")
    ap.add_argument("root", nargs="?", default=".", help="directory to scan (default: .)")
    ap.add_argument("--json", action="store_true", help="output JSON")
    a = ap.parse_args()
    st = count(a.root)
    if a.json:
        print(json.dumps(st, indent=2, sort_keys=True))
        return
    print(f"{'type':<10}{'files':>7}{'total':>8}{'code':>8}{'blank':>8}")
    for ext, s in sorted(st.items(), key=lambda x: -x[1]["total"]):
        print(f"{ext:<10}{s['files']:>7}{s['total']:>8}{s['code']:>8}{s['blank']:>8}")
    print(f"{'TOTAL':<10}{sum(s['files'] for s in st.values()):>7}"
          f"{sum(s['total'] for s in st.values()):>8}"
          f"{sum(s['code'] for s in st.values()):>8}"
          f"{sum(s['blank'] for s in st.values()):>8}")


if __name__ == "__main__":
    main()
