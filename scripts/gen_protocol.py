#!/usr/bin/env python3
"""Generate internal/protocol/table.json from docs/reference/protocol.json.

Usage: python3 scripts/gen_protocol.py [in.json] [out.json]

Only the fields the registry needs are kept. Free-text fields are dropped
when they mention internals of the vendor app (identifiers, event codes,
upload calls); the long notes are never copied. The status keeps only its
class (implemented-same, implemented-different, missing, n/a-N70). Output is
deterministic.
"""
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DEF_IN = os.path.join(HERE, "..", "docs/reference/protocol.json")
DEF_OUT = os.path.join(HERE, "..", "internal/protocol/table.json")

# Text that points at the vendor app's own code rather than at the wire.
LEAK = re.compile(
    r"(the vendor app|\bapp\b|EventBus|Bean|upload\w*|[a-z]+[A-Z]\w+\(|"
    r"\b[a-z]+[A-Z][A-Za-z]+\b|[A-Z_]{5,}|\w+\.\w+\(|SINGLEVALUE|"
    r"HTTPApi|Devicebind|\bJL\b|\bZR\b|vendorId|Anonymous|\b[A-Z][a-z]+[A-Z][A-Za-z]+\b|\b[A-Z]+:[A-Za-z]+)"
)
TEXT_FIELDS = ("frame", "responseOrNotify", "preconditions", "postActions")
KEEP = ("kind", "opcode", "opcodeHex", "name", "direction")


def clean_text(v):
    if not v or not isinstance(v, str):
        return ""
    v = v.strip()
    return "" if LEAK.search(v) else v


def status_class(s):
    s = (s or "").strip()
    m = re.match(r"[A-Za-z/0-9-]+", s)
    return m.group(0) if m else ""


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else DEF_IN
    dst = sys.argv[2] if len(sys.argv) > 2 else DEF_OUT
    with open(src) as f:
        rows = json.load(f)
    out = []
    for r in rows:
        e = {k: r.get(k) for k in KEEP}
        ch = (r.get("characteristic") or "").split(";")[0].strip()
        e["characteristic"] = ch
        for k in TEXT_FIELDS:
            e[k] = clean_text(r.get(k))
        e["models"] = (r.get("models") or "").strip()
        e["status"] = status_class(r.get("status"))
        out.append(e)
    with open(dst, "w") as f:
        json.dump(out, f, indent=1, ensure_ascii=False)
        f.write("\n")
    print("wrote %d entries to %s" % (len(out), dst))


if __name__ == "__main__":
    main()
