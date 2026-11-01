#!/usr/bin/env python3
"""csv2json: convert a CSV file (first row = header) to a JSON array of objects.

Usage: csv2json.py input.csv [-o output.json] [--indent N] [--delimiter ,] [--encoding utf-8]
Use '-' as input to read from stdin. Without -o, JSON goes to stdout.
"""
import argparse
import csv
import io
import json
import sys


def convert(text, delimiter=","):
    """Convert CSV text to a list of dicts. Empty input -> []."""
    reader = csv.reader(io.StringIO(text, newline=""), delimiter=delimiter)
    try:
        header = next(reader)
    except StopIteration:
        return []
    rows = []
    for row in reader:
        if not row:  # skip blank lines
            continue
        rec = {h: (row[i] if i < len(row) else "") for i, h in enumerate(header)}
        rows.append(rec)
    return rows


def build_parser():
    p = argparse.ArgumentParser(prog="csv2json", description="Convert CSV to JSON (array of objects).")
    p.add_argument("input", help="input CSV file, or '-' for stdin")
    p.add_argument("-o", "--output", help="output JSON file (default: stdout)")
    p.add_argument("--indent", type=int, default=2, help="JSON indent (default 2; 0 = compact)")
    p.add_argument("--delimiter", default=",", help="field delimiter (default ',')")
    p.add_argument("--encoding", default="utf-8", help="input encoding (default utf-8; BOM handled)")
    return p


def main(argv=None):
    args = build_parser().parse_args(argv)
    if len(args.delimiter) != 1:
        print("error: --delimiter must be a single character", file=sys.stderr)
        return 2
    try:
        # utf-8-sig strips a BOM if present and works for plain utf-8 too
        enc = "utf-8-sig" if args.encoding.lower().replace("_", "-") in ("utf-8", "utf8") else args.encoding
        if args.input == "-":
            data = sys.stdin.buffer.read()
        else:
            with open(args.input, "rb") as f:
                data = f.read()
        rows = convert(data.decode(enc), args.delimiter)
        out = json.dumps(rows, ensure_ascii=False, indent=args.indent if args.indent > 0 else None)
        if args.output:
            with open(args.output, "w", encoding="utf-8") as f:
                f.write(out + "\n")
        else:
            sys.stdout.write(out + "\n")
    except (OSError, UnicodeError, LookupError, csv.Error) as e:
        print("error: %s" % e, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
