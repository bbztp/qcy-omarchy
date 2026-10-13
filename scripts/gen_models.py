#!/usr/bin/env python3
"""Generate backend/internal/qcycore/models/models.json from the QCY web data.

Usage: python3 scripts/gen_models.py [product_list.json] [panels_dir] [out.json]

Inputs (not in the repo): the product list (earphones[]) and one
product/findControlPanels reply per vendorId. Images, HTML, URLs, pairing
texts and colours are dropped. Output is deterministic.
"""
import glob
import json
import os
import sys

HOME = os.path.expanduser("~")
REF = os.environ.get("QCY_PANELS_DIR", os.path.join(HOME, "qcy-panels"))
HERE = os.path.dirname(os.path.abspath(__file__))
DEF_PRODUCTS = os.path.join(REF, "product_list/extracted/en_prod_0.0.0.2.5.json")
DEF_PANELS = os.path.join(REF, "panels")
DEF_OUT = os.path.join(HERE, "..", "backend/internal/qcycore/models/models.json")


def num(v, default=None):
    """int or float from a number or numeric string, else default."""
    if isinstance(v, bool):
        return default
    if isinstance(v, (int, float)):
        return int(v) if float(v) == int(v) else float(v)
    if isinstance(v, str):
        try:
            f = float(v.strip())
            return int(f) if f == int(f) else f
        except (ValueError, OverflowError):
            return default
    return default


def inum(v, default=None):
    """Like num() but only whole numbers: the Go side declares these fields int."""
    n = num(v, None)
    if n is None or isinstance(n, float):
        return default
    return n


def csv(v):
    if v is None:
        return []
    if isinstance(v, (int, float)):
        return [num(v)]
    return [num(x, 0) for x in str(v).split(",") if x.strip() != ""]


def bands(d, top=None):
    top = top or {}
    freq = csv(d.get("freq") if d.get("freq") not in (None, "") else top.get("freq"))
    gain = csv(d.get("eqs"))
    q = csv(d.get("q") if d.get("q") not in (None, "") else top.get("q"))
    typ = csv(d.get("iitType"))
    out = []
    for i, f in enumerate(freq[: len(gain)] if gain else freq):
        out.append({
            "freq": f,
            "gain": gain[i] if i < len(gain) else 0,
            "q": q[i] if i < len(q) else (q[0] if len(q) == 1 else 0.7),
            "type": int(typ[i]) if i < len(typ) else 2,
        })
    return out


def effect(d, top):
    return {
        "id": inum(d.get("id")),
        "name": str(d.get("name", "")),
        "masterGain": num(d.get("mastergain"), 0),
        "bands": bands(d, top),
    }


def parse_eq(l):
    sys_eq = [effect(e, l) for e in l.get("sys_eq") or [] if isinstance(e, dict)]
    ind = l.get("individuation_eq")
    freq = csv(l.get("freq"))
    return {
        "bands": num(l.get("count"), len(freq)),
        "freq": freq,
        "minDb": num(l.get("mindb"), 0),
        "maxDb": num(l.get("maxdb"), 0),
        "baseDb": num(l.get("basedb"), 0),
        "sysEq": sys_eq,
        "individuation": effect(ind, l) if isinstance(ind, dict) else None,
    }


def parse_anc(l):
    def item(m):
        return {"name": str(m.get("name", "")), "start": inum(m.get("startcmdid")), "end": inum(m.get("endcmdid"))}

    modes = []
    for m in l.get("modes") or []:
        if not isinstance(m, dict):
            continue
        e = item(m)
        e["default"] = inum(m.get("defaultcmd"))
        e["items"] = [item(i) for i in m.get("items") or [] if isinstance(i, dict)]
        modes.append(e)
    return {"modes": modes}


def parse_keys(layout):
    """Flatten every key event (anything with an eventID and a list of
    {name, cmd}) from the three known layouts, by name only."""
    events, seen = [], set()

    def acts(lst):
        return [{"name": str(a.get("name", "")), "cmd": inum(a.get("cmd"))} for a in lst if isinstance(a, dict)]

    def walk(o, ctx):
        if isinstance(o, dict):
            name = o.get("keyName") or o.get("name") or ctx
            lst = o.get("list")
            if isinstance(lst, list) and lst and isinstance(lst[0], dict) and "cmd" in lst[0]:
                eid = inum(o.get("eventID"))
                key = (eid, str(name), json.dumps(acts(lst)))
                if key not in seen:
                    seen.add(key)
                    events.append({"name": str(name), "eventID": eid, "actions": acts(lst)})
            for k, v in o.items():
                if k == "list":
                    continue
                c = name
                if k in ("left", "right") and isinstance(v, dict):
                    c = "%s %s" % (k, name)
                walk(v, c)
        elif isinstance(o, list):
            for x in o:
                walk(x, ctx)

    walk(layout, "")
    return {"events": events} if events else None


def parse_panel(data):
    cp = data.get("controlPanel")
    if isinstance(cp, str):
        try:
            cp = json.loads(cp)
        except ValueError:
            cp = None
    if not isinstance(cp, dict):
        return None
    p = {"anc": None, "eq": None, "autoOff": None, "wear": None, "beep": None,
         "fit": None, "balance": False, "find": False, "keys": None, "other": []}
    for l in cp.get("layouts") or []:
        if not isinstance(l, dict):
            continue
        t = inum(l.get("type"))
        cmd = inum(l.get("cmdid", l.get("cmdID")))
        if t == 9 and p["anc"] is None:
            p["anc"] = parse_anc(l)
        elif t == 2 and p["eq"] is None:
            p["eq"] = parse_eq(l)
        elif t == 102 and p["autoOff"] is None:
            p["autoOff"] = {"cmdId": cmd, "repeat": inum(l.get("repeat"))}
        elif t == 104 and p["wear"] is None:
            s = l.get("sensitivity")
            sens = None
            if isinstance(s, dict):
                sens = {"cmdId": inum(s.get("cmdID", s.get("cmdid"))),
                        "items": [{"name": str(i.get("name", "")), "cid": num(i.get("cid"))}
                                  for i in s.get("items") or [] if isinstance(i, dict)]}
            p["wear"] = {"cmdId": cmd,
                         "subItems": [{"name": str(i.get("name", "")), "cid": num(i.get("cid"))}
                                      for i in l.get("subItem") or [] if isinstance(i, dict)],
                         "sensitivity": sens}
        elif t == 103 and p["beep"] is None:
            names = l.get("itemName") if isinstance(l.get("itemName"), dict) else {}
            items = [{"value": inum(k, 0), "name": str(v)} for k, v in names.items()]
            items.sort(key=lambda i: i["value"])
            p["beep"] = {"cmdId": cmd, "items": items}
        elif t == 110 and p["fit"] is None:
            p["fit"] = {"cmdId": cmd}
        elif t == 8:
            p["balance"] = True
        elif t == 3:
            p["find"] = True
        elif t in (10, 1, 14):
            if p["keys"] is None:
                p["keys"] = parse_keys(l)
        elif t == 4:
            for it in l.get("items") or []:
                if isinstance(it, dict):
                    p["other"].append({"type": 4, "name": str(it.get("title", it.get("name", ""))),
                                       "cmdId": inum(it.get("cmdid"))})
        elif t in (101, 1001, 1000) and t != 1000:
            p["other"].append({"type": t, "name": str(l.get("name", "")), "cmdId": cmd})
        else:
            p["other"].append({"type": t, "name": str(l.get("name", l.get("title", "")) or ""), "cmdId": cmd})
    p["other"].sort(key=lambda o: (o["type"] if o["type"] is not None else -1, o["name"], o["cmdId"] if o["cmdId"] is not None else -1))
    return p


def main(argv):
    products = argv[1] if len(argv) > 1 else DEF_PRODUCTS
    panels = argv[2] if len(argv) > 2 else DEF_PANELS
    out = argv[3] if len(argv) > 3 else DEF_OUT
    models = {}
    with open(products, encoding="utf-8") as f:
        for e in json.load(f).get("earphones", []):
            vid = inum(e.get("vendorID"))
            if vid is None:
                continue
            models[str(vid)] = {
                "title": str(e.get("title", "")),
                "subTitle": str(e.get("subTitle", "")),
                "family": str(e.get("classification", "")),
                "bluetoothVer": num(e.get("bluetoothVer"), 0),
                "hasSwitch": bool(num(e.get("hasSwitch"), 0)),
                "firmware": None,
                "panel": None,
            }
    for path in sorted(glob.glob(os.path.join(panels, "*.json"))):
        with open(path, encoding="utf-8") as f:
            d = json.load(f)
        vid = inum(d.get("_vendorId"))
        if vid is None:
            vid = num(os.path.splitext(os.path.basename(path))[0])
        if vid is None:
            continue
        m = models.setdefault(str(vid), {
            "title": str(d.get("_title", "")), "subTitle": "", "family": "",
            "bluetoothVer": 0, "hasSwitch": False, "firmware": None, "panel": None})
        data = d.get("data")
        if isinstance(data, dict):
            m["firmware"] = {"gte": data.get("firmwareversionGte"), "lte": data.get("firmwareversionLte")}
            m["panel"] = parse_panel(data)
    with open(out, "w", encoding="utf-8") as f:
        json.dump(models, f, indent=1, sort_keys=True, ensure_ascii=False)
        f.write("\n")
    print("%d models, %d with panel -> %s" % (len(models), sum(1 for m in models.values() if m["panel"]), out))


if __name__ == "__main__":
    main(sys.argv)
