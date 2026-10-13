#!/usr/bin/env python3
"""Generate internal/qcycore/models/models.json from the QCY web data.

Usage: QCY_PANELS_DIR=<dir> python3 scripts/gen_models.py [product_list.json] [panels_dir] [out.json]

QCY_PANELS_DIR holds product_list/extracted/en_prod_0.0.0.2.5.json and
panels/<vendorId>.json (defaults: <dir>/product_list/..., <dir>/panels).
Writes schemaVersion 2: {"schemaVersion":2,"models":{<vendorId>:{...}}}, one
model per line; every v1 field is unchanged, panel.functions is new.

Inputs (not in the repo): the product list (earphones[]) and one
product/findControlPanels reply per vendorId. HTML, pairing texts and
other URLs are dropped; only product image URLs on img.qcymall.com are kept
(per colour, plus the product's default set). Output is deterministic.
"""
import glob
import json
import os
import re
import sys
from urllib.parse import urlsplit

IMAGE_HOST = "img.qcymall.com"
SCHEMA_VERSION = 2

HOME = os.path.expanduser("~")
REF = os.environ.get("QCY_PANELS_DIR", os.path.join(HOME, "qcy-panels"))
HERE = os.path.dirname(os.path.abspath(__file__))
DEF_PRODUCTS = os.path.join(REF, "product_list/extracted/en_prod_0.0.0.2.5.json")
DEF_PANELS = os.path.join(REF, "panels")
DEF_OUT = os.path.join(HERE, "..", "internal/qcycore/models/models.json")


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


def img(v):
    """An http(s) URL on the QCY image CDN, else None."""
    if not isinstance(v, str):
        return None
    v = v.strip()
    try:
        u = urlsplit(v)
        host = u.hostname
        u.port  # raises on a malformed port
    except ValueError:
        return None
    if u.scheme not in ("http", "https") or host != IMAGE_HOST or u.username or u.password:
        return None
    return v


def image_set(d):
    return {"icon": img(d.get("icon")), "left": img(d.get("leftImg")),
            "right": img(d.get("rightImg")), "box": img(d.get("boxImg"))}


def colors(e):
    out = []
    for c in e.get("colors") or []:
        if not isinstance(c, dict):
            continue
        idx = inum(c.get("colorIndex", c.get("colorlndex")))
        entry = {"name": str(c.get("name", "")), "index": idx, "hide": bool(num(c.get("hide"), 0))}
        entry.update(image_set(c))
        out.append(entry)
    return out


def csv(v):
    if v is None:
        return []
    if isinstance(v, (int, float)):
        return [num(v)]
    return [fnum(x) for x in str(v).split(",") if x.strip() != ""]


def fnum(x):
    """A number from a list cell; '1k' / '2.5k' mean x1000 (frequency lists)."""
    t = str(x).strip()
    if t[-1:] in ("k", "K"):
        n = num(t[:-1], None)
        return num(n * 1000, 0) if n is not None else 0
    return num(t, 0)


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


# ---- schema v2: per-function declarations ---------------------------------

UUID_SUFFIX = "-0000-1000-8000-00805f9b34fb"
HEX_RE = re.compile(r"^(?:[0-9A-Fa-f]{2})+$")
HTML_RE = re.compile(r"<[A-Za-z/!][^>]*>")
URL_RE = re.compile(r"^\s*(?:https?:)?//", re.I)
# keys that only point at UI artwork, sounds or HTML help: not carried
ASSET_KEY_RE = re.compile(r"(?i)(img|image|icon|mp3url|^info$)")
# server text that only tells the vendor app to update itself
NOISE_MSG = "Please upgrade the APP to the latest version."


def uuid_short(v):
    """GATT characteristic: the 32-bit part when it uses the base UUID."""
    if not isinstance(v, str) or not v.strip():
        return None
    s = v.strip()
    if s.lower().endswith(UUID_SUFFIX):
        s = s[: -len(UUID_SUFFIX)]
    return s.upper()


def hexs(v):
    """Upper-case hex string of whole bytes, else None."""
    if isinstance(v, bool) or v is None:
        return None
    s = str(v).strip()
    return s.upper() if HEX_RE.match(s) else None


def junk_str(x):
    return isinstance(x, str) and (URL_RE.match(x) is not None or HTML_RE.search(x) is not None)


def clean(v):
    """Sanitised copy of a JSON value: no artwork keys, URLs or HTML."""
    if isinstance(v, dict):
        out = {}
        for k, x in v.items():
            if ASSET_KEY_RE.search(str(k)) or junk_str(x):
                continue
            out[str(k)] = clean(x)
        return out
    if isinstance(v, list):
        return [clean(x) for x in v if not junk_str(x)]
    return v


def scalar(v):
    if isinstance(v, bool) or v is None:
        return v
    if isinstance(v, (int, float)):
        return v
    if isinstance(v, str):
        n = num(v, None)
        return n if n is not None and re.match(r"^-?[0-9.]+$", v.strip()) else v
    return None


def text(v):
    return v if isinstance(v, str) and v.strip() and not junk_str(v) and v != NOISE_MSG else None


def put(d, k, v):
    if v is None or v == "" or v == [] or v == {}:
        return
    d[k] = v


MESSAGE_KEYS = [("msg", "msg"), ("msg_ios", "msgIos"), ("msg_android", "msgAndroid"),
                ("rebootTip", "rebootTip"), ("alarmMsg", "alarmMsg"), ("alarmMsg_ios", "alarmMsgIos"),
                ("confTip", "confTip"), ("toastTip", "toastTip"), ("toastTip2", "toastTip2"),
                ("ancMsg", "ancMsg"), ("desc", "desc"), ("subTitle", "subTitle"),
                ("valueTitle", "valueTitle"), ("noSpaceTip", "noSpaceTip")]


def take_messages(d):
    """Pop the user-facing texts out of d (msg, rebootTip, toastMsg ...)."""
    m = {}
    for src, dst in MESSAGE_KEYS:
        if src in d:
            t = text(d.pop(src))
            put(m, dst, t)
    if "alert" in d:
        a = scalar(d.pop("alert"))
        if a not in (None, "", 0, False):
            m["alert"] = a
    if "toastMsg" in d:
        put(m, "toastMsg", clean(d.pop("toastMsg")))
    return m


def parse_choice(o, hex_key=None):
    """A selectable option: label plus the value (number) or bytes (hex)."""
    c = {"label": str(o.get("name", o.get("title", "")))}
    for k in ("cid", "index"):
        if k in o and inum(o.get(k)) is not None:
            c["value"] = inum(o.get(k))
            break
    h = hexs(o.get(hex_key)) if hex_key else None
    if h is None:
        for k in ("key", "cmd"):
            if k in o and hexs(o.get(k)) is not None:
                h = hexs(o.get(k))
                break
    put(c, "bytes", h)
    put(c, "describe", text(o.get("describe")))
    return c


def parse_standby(opts):
    out = []
    for label, h in opts:
        e = {"label": str(label), "bytes": h}
        b = bytes.fromhex(h)
        if len(b) == 2 and h != "FFFF":
            e["minutes"] = int.from_bytes(b, "little")
        elif h == "FFFF":
            e["never"] = True
        out.append(e)
    return {"options": out}


def eq_effect(d, top):
    e = effect(d, top)
    v = csv(d.get("vals")) if d.get("vals") not in (None, "") else []
    put(e, "vals", v)
    if d.get("onlyTws") is True:
        e["onlyTws"] = True
    put(e, "alarmMsg", text(d.get("alarmMsg")))
    put(e, "alarmMsgIos", text(d.get("alarmMsg_ios")))
    sd = d.get("soundDB")
    if sd not in (None, ""):
        e["soundDB"] = csv(sd)
    return e


def eq_block(l):
    freq = csv(l.get("freq"))
    b = {"count": inum(l.get("count")),
         "freq": freq, "minDb": num(l.get("mindb")), "maxDb": num(l.get("maxdb"))}
    if l.get("basedb") not in (None, ""):
        b["baseDb"] = num(l.get("basedb"))
    if l.get("q") not in (None, ""):
        b["q"] = csv(l.get("q"))
    if l.get("iitType") not in (None, ""):
        b["iitType"] = [int(x) for x in csv(l.get("iitType"))]
    if l.get("volumelevel") not in (None, ""):
        b["volumeLevel"] = num(l.get("volumelevel"))
    if inum(l.get("supportMaxCount")) is not None:
        b["supportMaxCount"] = inum(l.get("supportMaxCount"))
    put(b, "rebootTip", text(l.get("rebootTip")))
    b["effects"] = [eq_effect(e, l) for e in l.get("sys_eq") or [] if isinstance(e, dict)]
    ind = l.get("individuation_eq")
    if isinstance(ind, dict):
        b["individuation"] = eq_effect(ind, l)
    aux = l.get("aux_eq")
    if isinstance(aux, list):
        put(b, "auxEq", [eq_effect(e, l) for e in aux if isinstance(e, dict)])
    return {k: v for k, v in b.items() if v is not None}


ANC_KEYS = [("name", "name", str), ("startcmdid", "start", inum), ("endcmdid", "end", inum),
            ("defaultcmd", "default", inum), ("cmdid", "cmdId", inum), ("viewtype", "viewType", inum),
            ("voicecmdid", "voiceCmdId", inum), ("ancInvert", "ancInvert", inum),
            ("progress", "progress", inum), ("ancindex", "ancIndex", inum)]


def anc_node(m):
    n = {}
    for src, dst, fn in ANC_KEYS:
        if src in m:
            v = fn(m[src]) if fn is not str else str(m[src])
            if v is not None:
                n[dst] = v
    if isinstance(m.get("envAdaptation"), list):
        n["envAdaptation"] = [str(x) for x in m["envAdaptation"]]
    for src in ("envName", "describe", "noSpaceTip"):
        put(n, src, text(m.get(src)))
    if m.get("hidePop") is True:
        n["hidePop"] = True
    if m.get("touchDisable") not in (None, "", 0, False):
        n["touchDisable"] = scalar(m["touchDisable"])
    if m.get("valueDescribes") not in (None, "", [], {}):
        n["valueDescribes"] = clean(m["valueDescribes"])
    items = [anc_node(i) for i in m.get("items") or [] if isinstance(i, dict)] if isinstance(m.get("items"), list) else []
    put(n, "items", items)
    return n


def keys_block(l, legacy):
    """Every key event with its context: group (music / voice / sdMode /
    buttons / actions), side, key name, characteristic, factory action."""
    events, seen = [], set()

    def acts(lst):
        out = []
        for a in lst:
            if not isinstance(a, dict):
                continue
            c = a.get("cmd")
            e = {"name": str(a.get("name", ""))}
            if isinstance(c, str) and legacy:
                h = hexs(c)
                put(e, "bytes", h)
                e["cmd"] = int(h, 16) if h else None
            else:
                e["cmd"] = inum(c)
            out.append(e)
        return out

    def walk(o, name, ctx):
        if isinstance(o, dict):
            nm = o.get("keyName") or o.get("name") or name
            c2 = dict(ctx)
            if "keyName" in o:
                c2["keyName"] = str(o["keyName"])
            if inum(o.get("keyID")) is not None:
                c2["keyId"] = inum(o.get("keyID"))
            lst = o.get("list")
            if isinstance(lst, list) and lst and isinstance(lst[0], dict) and "cmd" in lst[0]:
                ev = {"name": str(nm), "eventID": inum(o.get("eventID")), "actions": acts(lst)}
                for k in ("group", "side", "keyName", "keyId"):
                    put(ev, k, c2.get(k))
                put(ev, "character", uuid_short(o.get("character")))
                if inum(o.get("defaultFun")) is not None:
                    ev["defaultFun"] = inum(o.get("defaultFun"))
                put(ev, "default", text(o.get("default")) or (text(o.get("music"))))
                put(ev, "voice", text(o.get("voice")))
                put(ev, "showTitle", text(o.get("showTitle")))
                put(ev, "helpTip", text(o.get("helpTip")))
                key = json.dumps(ev, sort_keys=True)
                if key not in seen:
                    seen.add(key)
                    events.append(ev)
            for k, v in o.items():
                if k == "list":
                    continue
                c3 = c2
                nm2 = nm
                if k in ("left", "right") and isinstance(v, dict):
                    c3 = dict(c2, side=k)
                    nm2 = "%s %s" % (k, nm)
                walk(v, nm2, c3)
        elif isinstance(o, list):
            for x in o:
                walk(x, name, ctx)

    groups = {}
    for k, v in l.items():
        if k in ("music", "voice", "sdMode", "buttons", "actions") and isinstance(v, (dict, list)):
            walk(v, "", {"group": k})
            if isinstance(v, dict) and text(v.get("name")):
                groups[k] = v["name"]
        elif k in ("left", "right") and isinstance(v, dict):
            walk(v, "", {"group": "legacy", "side": k})
    b = {}
    put(b, "events", events)
    put(b, "groups", groups)
    vo = l.get("voice")
    if isinstance(vo, dict) and isinstance(vo.get("event"), list):
        ve = []
        for e in vo["event"]:
            if isinstance(e, dict):
                x = {"name": str(e.get("name", ""))}
                put(x, "left", text(e.get("left")))
                put(x, "right", text(e.get("right")))
                ve.append(x)
        put(b, "voice", ve)
    return b


def finish(f, d, known_blocks_done=True):
    """Leftover keys: scalars to props, structures to raw (sanitised)."""
    props, raw = {}, {}
    for k, v in d.items():
        if ASSET_KEY_RE.search(str(k)) or junk_str(v):
            continue
        if isinstance(v, (dict, list)):
            c = clean(v)
            if c not in ({}, []):
                raw[str(k)] = c
        else:
            s = scalar(v)
            if s is not None and s != "":
                props[str(k)] = s
    put(f, "props", props)
    put(f, "raw", raw)
    return f


def build_function(src, is_item):
    d = dict(src)
    f = {}
    t = inum(d.pop("type", None))
    if t is not None:
        f["type"] = t
    cmd = inum(d.pop("cmdid", d.pop("cmdID", None)))
    d.pop("cmdID", None)
    put(f, "cmdId", cmd)
    nm = d.pop("name", None)
    title = d.pop("title", None)
    put(f, "name", text(nm) or text(title))
    if is_item or t in (20, 24):
        if text(nm) and text(title):
            d["title"] = title
    ch = d.pop("character", None)
    put(f, "character", uuid_short(ch))
    msgs = take_messages(d)

    if t is None:
        for k, v in src.items():
            pass
        f.pop("character", None)
        return finish(f, src)
    if t in (200, 201, 203, 204, 206) or (t == 2 and is_item and any(
            isinstance(x, dict) and "boolVal" in x for x in d.get("sub_items") or [])):
        on, off = [], []
        for x in d.pop("sub_items", None) or []:
            if isinstance(x, dict) and hexs(x.get("cmd")):
                (on if x.get("boolVal") is True else off).append(hexs(x["cmd"]))
        tg = {}
        put(tg, "on", on)
        put(tg, "off", off)
        f["toggle"] = tg
    elif is_item and t == 2:
        put(f, "choices", [parse_choice(x) for x in d.pop("sub_items", None) or [] if isinstance(x, dict)])
    elif is_item and t in (3, 4, 100):
        c = {}
        put(c, "bytes", hexs(d.pop("cmd", None)))
        f["command"] = c
    elif not is_item and t in (2, 23):
        f["eq"] = eq_block(d)
        for k in ("sys_eq", "individuation_eq", "aux_eq", "freq", "q", "iitType", "eqs", "count", "mindb",
                  "maxdb", "basedb", "volumelevel", "supportMaxCount", "modes"):
            d.pop(k, None)
    elif not is_item and t in (6, 9, 11):
        f["anc"] = {"modes": [anc_node(m) for m in d.pop("modes", None) or [] if isinstance(m, dict)]}
    elif not is_item and t in (1, 10, 14, 114):
        kb = keys_block(d, t == 1)
        f["keys"] = kb
        for k in ("left", "right", "music", "voice", "sdMode", "buttons", "actions"):
            d.pop(k, None)
    elif not is_item and t in (102, 1000) and isinstance(d.get("itemName"), dict) and d.get("subItem") is None \
            and cmd in (20, 31) and all(hexs(k) for k in d["itemName"]):
        f["standby"] = parse_standby([(v, hexs(k)) for k, v in d.pop("itemName").items()])
        d.pop("itemKeyIos", None)
        d.pop("itemNameIos", None)
    elif not is_item and t in (102, 1000) and cmd in (20, 31) and isinstance(d.get("subItem"), list) \
            and all(isinstance(x, dict) and hexs(x.get("key")) for x in d["subItem"]):
        f["standby"] = parse_standby([(x.get("name", ""), hexs(x["key"])) for x in d.pop("subItem")])
        d.pop("itemName", None)
        d.pop("itemKeyIos", None)
        d.pop("itemNameIos", None)
    elif not is_item and t == 103 and isinstance(d.get("itemName"), dict):
        ch = []
        for k, v in d.pop("itemName").items():
            e = {"label": str(v)}
            n = inum(k)
            if n is not None:
                e["value"] = n
            ch.append(e)
        f["choices"] = ch
    elif not is_item and t == 104 and isinstance(d.get("subItem"), list):
        f["choices"] = [parse_choice(x) for x in d.pop("subItem") if isinstance(x, dict)]
        s = d.pop("sensitivity", None)
        if isinstance(s, dict):
            sec = {}
            put(sec, "cmdId", inum(s.get("cmdID", s.get("cmdid"))))
            put(sec, "name", text(s.get("name")))
            put(sec, "choices", [parse_choice(x) for x in s.get("items") or [] if isinstance(x, dict)])
            f["secondary"] = sec
    elif not is_item and t in (111, 116) and isinstance(d.get("subItem"), list):
        f["choices"] = [parse_choice(x) for x in d.pop("subItem") if isinstance(x, dict)]
    elif not is_item and t == 1000 and isinstance(d.get("itemName"), dict):
        sub = d.pop("subItem", None)
        d.pop("itemKeyIos", None)
        d.pop("itemNameIos", None)
        src_items = sub if isinstance(sub, list) else [{"key": k, "name": v} for k, v in d.pop("itemName").items()]
        d.pop("itemName", None)
        f["choices"] = [parse_choice(x) for x in src_items if isinstance(x, dict)]
    elif not is_item and t == 1001 and isinstance(d.get("itemKey"), list):
        keys, names = d.pop("itemKey"), d.pop("itemName", None) or []
        opts = []
        for i, k in enumerate(keys):
            e = {"label": str(names[i]) if i < len(names) else ""}
            put(e, "bytes", hexs(k))
            opts.append(e)
        lg = {"options": opts}
        langs = d.pop("languages", None)
        if isinstance(langs, str):
            put(lg, "languages", [x.strip() for x in langs.split(",") if x.strip()])
        f["language"] = lg
    elif not is_item and t == 24:
        fi = d.pop("funcIDs", None)
        if isinstance(fi, str):
            d["funcIDs"] = [inum(x) for x in fi.split(",") if inum(x) is not None]
    elif not is_item and t == 20:
        pass
    put(f, "messages", msgs)
    return finish(f, d)


def parse_functions(cp):
    out = []
    for l in cp.get("layouts") or []:
        if not isinstance(l, dict):
            continue
        if inum(l.get("type")) == 4:
            items = l.get("items")
            if isinstance(items, dict):
                items = list(items.values())
            for it in items or []:
                if isinstance(it, dict):
                    out.append(build_function(it, True))
            continue
        out.append(build_function(l, False))
    return out


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
         "fit": None, "balance": False, "find": False, "keys": None, "other": [],
         "functions": parse_functions(cp)}
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
                "images": image_set(e),
                "colors": colors(e),
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
            "bluetoothVer": 0, "hasSwitch": False, "firmware": None, "panel": None,
            "images": {"icon": None, "left": None, "right": None, "box": None}, "colors": []})
        data = d.get("data")
        if isinstance(data, dict):
            m["firmware"] = {"gte": data.get("firmwareversionGte"), "lte": data.get("firmwareversionLte")}
            m["panel"] = parse_panel(data)
    with open(out, "w", encoding="utf-8") as f:
        # one model per line: compact, but a diff still points at the model
        f.write('{"schemaVersion":%d,"models":{\n' % SCHEMA_VERSION)
        keys = sorted(models)
        for i, k in enumerate(keys):
            f.write(json.dumps(k) + ":" + json.dumps(models[k], sort_keys=True, ensure_ascii=False,
                                                      separators=(",", ":")))
            f.write(",\n" if i + 1 < len(keys) else "\n")
        f.write("}}\n")
    print("%d models, %d with panel -> %s" % (len(models), sum(1 for m in models.values() if m["panel"]), out))


if __name__ == "__main__":
    main(sys.argv)
