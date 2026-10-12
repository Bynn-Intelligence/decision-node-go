#!/usr/bin/env python3
"""Fails when a file about to be published carries what a public package
must not: a path into internal documents, a local home directory, an
attribution line, or one of the words kept below as SHA-256 hashes.

    check-published.py [--readme FILE] FILE...

--readme also checks FILE for the boosters it must not list. Binary files
are skipped. Each hit is printed as file:line and what kind of hit it is,
never the matched text.
"""

import hashlib
import re
import sys

# Each pattern is written so that it does not match its own source text.
PATTERNS = [
    ("a path into internal documents", re.compile(r"docs/[0-9]{2}|backend/[0-9]{2}|product[s]/", re.I)),
    ("a local home directory", re.compile(r"/user[s]/", re.I)),
    ("an attribution line", re.compile(r"co-authored-b[y]|generated[ ]with", re.I)),
]

# SHA-256 of lowercase words that must not appear anywhere, keyed by length:
# inside a longer word as well as alone.
WITHIN = {
    6: {
        "9e9b2efac0b62e4cf6f08cc6b7095e6ad7e18c447458c5849accb698f7149ef3",
        "b7933ed6180d9e2895a9d2df885e2a8b92e998a3c16297581f5b8698daacf492",
        "31d1b3d540ecf85228db7797efbe7d198ec5406abd3f6e64f27f684e9a312d29",
    },
    7: {
        "934a11e602682e862b4b4d1f881ae8da1e097e3f6964bd780e5bb1fb798a6fdd",
    },
}
# SHA-256 of lowercase words that must not appear as a whole word.
WHOLE = {
    "2e0b8d61fa2a6959d254b6ff5d0fb512249329097336a35568089933b49abdde",
}

# Boosters the README does not list.
README_BOOSTERS = re.compile(r"\b(face_detection|wanted_person)\b")

WORD = re.compile(r"\w+")


def digest(word):
    return hashlib.sha256(word.encode()).hexdigest()


def named(line):
    for m in WORD.finditer(line.lower()):
        word = m.group()
        if digest(word) in WHOLE:
            return True
        for n, hashes in WITHIN.items():
            for i in range(len(word) - n + 1):
                if digest(word[i : i + n]) in hashes:
                    return True
    return False


def check(path, readme):
    with open(path, "rb") as f:
        data = f.read()
    if b"\0" in data[:8192]:
        return []
    hits = []
    for number, line in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
        for kind, pattern in PATTERNS:
            if pattern.search(line):
                hits.append(f"{path}:{number}: {kind}")
        if named(line):
            hits.append(f"{path}:{number}: a word that must not be published")
        if readme and README_BOOSTERS.search(line):
            hits.append(f"{path}:{number}: a booster the README does not list")
    return hits


def main(args):
    readme = None
    if len(args) >= 2 and args[0] == "--readme":
        readme, args = args[1], args[2:]
        args = [readme] + [a for a in args if a != readme]
    if not args:
        print("usage: check-published.py [--readme FILE] FILE...", file=sys.stderr)
        return 2
    hits = []
    for path in args:
        hits += check(path, path == readme)
    for hit in hits:
        print(f"::error::{hit}")
    print(f"checked {len(args)} files, {len(hits)} hits")
    return 1 if hits else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
