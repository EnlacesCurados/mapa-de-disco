#!/usr/bin/env python3
"""Mapa de Disco: a visual disk space analyzer (Python edition).

Same interface and same local API as the Go executable, for computers where an
unsigned .exe cannot run (for example with Windows Smart App Control on).

Run:  python mapadisco.py
Needs Python 3.8 or newer. No extra packages.
"""
import ctypes
import hashlib
import heapq
import json
import os
import queue
import secrets
import shutil
import subprocess
import sys
import threading
import time
import webbrowser
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

VERSION = "1.2.0"
WIN = os.name == "nt"
SEP = os.sep
HERE = os.path.dirname(os.path.abspath(__file__))
NCATS = 8

# Categories: 0 video, 1 image, 2 audio, 3 documents, 4 archives, 5 code and data, 6 programs and system, 7 other
_GROUPS = {
    0: "mp4 mov avi mkv wmv flv webm m4v mpg mpeg mts m2ts 3gp vob mxf r3d braw prores ts ogv insv lrv dv",
    1: "jpg jpeg png gif bmp tif tiff webp heic heif raw cr2 cr3 nef arw dng orf rw2 psd psb ai svg ico exr hdr tga eps indd xcf kra avif dpx",
    2: "mp3 wav flac aac ogg m4a wma aiff aif opus mid midi amr ape alac",
    3: "pdf doc docx xls xlsx ppt pptx odt ods odp txt rtf md epub mobi csv pages numbers key one msg eml djvu tex",
    4: "zip rar 7z tar gz bz2 xz tgz iso cab dmg zst img vhd vhdx wim esd",
    5: "js ts jsx tsx py go rs c cpp h hpp java cs php rb html htm css scss json xml yaml yml sql db sqlite sqlite3 mdb accdb ipynb sh bat ps1 lua swift kt safetensors ckpt pt pth gguf onnx bin dat pak npy npz parquet log",
    6: "exe dll sys msi msp msix appx drv ocx cpl scr com lnk apk jar so dylib mui cat inf pdb lib obj tmp etl dmp",
}
EXT_CAT = {e: c for c, words in _GROUPS.items() for e in words.split()}


def cat_of(name):
    i = name.rfind(".")
    if i < 0 or i == len(name) - 1:
        return 7
    return EXT_CAT.get(name[i + 1:].lower(), 7)


class Dir:
    """A folder. Files are plain tuples (name, size, mtime, cat) to save memory."""
    __slots__ = ("name", "size", "mtime", "children", "files", "cats", "delta")

    def __init__(self, name, mtime=0):
        self.name, self.mtime = name, mtime
        self.size, self.files, self.children, self.cats, self.delta = 0, 0, [], [0] * NCATS, 0


def nsize(n):
    return n.size if isinstance(n, Dir) else n[1]


def nname(n):
    return n.name if isinstance(n, Dir) else n[0]


ATTR_REPARSE, ATTR_OFFLINE, ATTR_RECALL = 0x400, 0x1000, 0x400000
LINK_TAGS = (0xA0000003, 0xA000000C)  # junction, symbolic link


class Scan:
    def __init__(self, root=""):
        self.root, self.tree, self.state = root, None, "idle"
        self.started, self.elapsed = time.time(), 0.0
        self.files = self.dirs = self.bytes = self.errors = self.cloud = self.trashed = self.prev_time = 0
        self.current = root
        self.lock = threading.Lock()

    def walk(self, path, node, todo):
        self.current = path
        files = size = cloud = 0
        try:
            with os.scandir(path) as it:
                for e in it:
                    try:
                        if e.is_symlink():
                            continue
                        st = e.stat(follow_symlinks=False)
                        attrs = getattr(st, "st_file_attributes", 0)
                        if e.is_dir(follow_symlinks=False):
                            # Junctions and directory links are skipped; OneDrive folders are scanned.
                            if attrs & ATTR_REPARSE and getattr(st, "st_reparse_tag", 0) in LINK_TAGS:
                                continue
                            child = Dir(e.name, int(st.st_mtime))
                            node.children.append(child)
                            todo.put((e.path, child))
                        elif e.is_file(follow_symlinks=False) or attrs:
                            if attrs & (ATTR_OFFLINE | ATTR_RECALL):  # cloud-only: takes no disk space
                                cloud += st.st_size
                                continue
                            node.children.append((e.name, st.st_size, int(st.st_mtime), cat_of(e.name)))
                            files += 1
                            size += st.st_size
                    except OSError:
                        continue
        except OSError:
            with self.lock:
                self.errors += 1
            return
        with self.lock:
            self.files += files
            self.bytes += size
            self.cloud += cloud
            self.dirs += 1

    def run(self):
        tree = Dir(self.root)
        todo = queue.Queue()
        todo.put((self.root, tree))

        def worker():
            while True:
                item = todo.get()
                if item is None:
                    return
                try:
                    self.walk(item[0], item[1], todo)
                finally:
                    todo.task_done()

        workers = [threading.Thread(target=worker, daemon=True) for _ in range(8)]
        for w in workers:
            w.start()
        todo.join()
        for _ in workers:
            todo.put(None)
        aggregate(tree)
        try:
            apply_history(self, tree)
        except (OSError, ValueError):
            pass
        with LOCK:
            self.tree = tree
            self.elapsed = time.time() - self.started
            self.state = "done"


def aggregate(root):
    """Sizes, file counts and per-type totals, bottom-up, without recursion."""
    order, stack = [], [root]
    while stack:
        d = stack.pop()
        order.append(d)
        stack.extend(c for c in d.children if isinstance(c, Dir))
    for d in reversed(order):
        size = files = 0
        cats = [0] * NCATS
        for c in d.children:
            if isinstance(c, Dir):
                size += c.size
                files += c.files
                for i in range(NCATS):
                    cats[i] += c.cats[i]
            else:
                size += c[1]
                files += 1
                cats[c[3]] += c[1]
        d.size, d.files, d.cats = size, files, cats
        d.children.sort(key=nsize, reverse=True)


LOCK = threading.RLock()
cur = Scan()


def norm(p):
    return os.path.normcase(os.path.normpath(p))


def clean(p):
    p = os.path.normpath(p)
    if WIN and len(p) == 2 and p[1] == ":":
        p += SEP
    return p


def find_node(scan, path):
    """Returns (node, chain of ancestors including the root), or (None, None)."""
    if scan.tree is None:
        return None, None
    path = clean(path)
    chain = [scan.tree]
    if norm(path) == norm(scan.root):
        return scan.tree, chain
    prefix = scan.root if scan.root.endswith(SEP) else scan.root + SEP
    if len(path) <= len(prefix) or norm(path[:len(prefix)]) != norm(prefix):
        return None, None
    node = scan.tree
    for part in path[len(prefix):].split(SEP):
        if not isinstance(node, Dir):
            return None, None
        nxt = next((c for c in node.children if nname(c) == part), None)
        if nxt is None:
            return None, None
        node = nxt
        chain.append(node)
    return node, chain


def prune(n, minimum, depth, budget):
    if not isinstance(n, Dir):
        return {"n": n[0], "s": n[1], "c": n[3], "m": n[2]}
    o = {"n": n.name, "s": n.size, "c": 0, "m": n.mtime, "d": 1, "f": n.files}
    if abs(n.delta) >= 10 * MB:
        o["g"] = n.delta
    if depth <= 0 or budget[0] <= 0:
        return o
    ch = []
    for i, c in enumerate(n.children):
        s = nsize(c)
        if s < minimum or s == 0 or budget[0] <= 0:
            rest = sum(nsize(r) for r in n.children[i:])
            if rest > 0:
                ch.append({"s": rest, "r": len(n.children) - i, "c": 7})
            break
        budget[0] -= 1
        ch.append(prune(c, minimum, depth - 1, budget))
    if ch:
        o["ch"] = ch
    return o


def top_files(node, path, cat, limit=100):
    out, n, heap = [], 0, [(-nsize(node), 0, node, path)]
    while heap and len(out) < limit:
        _, _, it, p = heapq.heappop(heap)
        if not isinstance(it, Dir):
            out.append({"p": p, "s": it[1], "c": it[3], "m": it[2]})
            continue
        for c in it.children:
            s = nsize(c)
            if s == 0:
                break
            if cat >= 0:
                if isinstance(c, Dir):
                    if c.cats[cat] == 0:
                        continue
                elif c[3] != cat:
                    continue
            n += 1
            heapq.heappush(heap, (-s, n, c, os.path.join(p, nname(c))))
    return out


def remove_node(scan, path):
    node, chain = find_node(scan, path)
    if node is None or len(chain) < 2:
        return
    chain[-2].children.remove(node)
    if isinstance(node, Dir):
        size, files, cats = node.size, node.files, node.cats
    else:
        size, files, cats = node[1], 1, [node[1] if i == node[3] else 0 for i in range(NCATS)]
    for a in chain[:-1]:
        a.size -= size
        if scan.prev_time:
            a.delta -= size
        a.files -= files
        for i in range(NCATS):
            a.cats[i] -= cats[i]
    for a in chain[:-2]:
        a.children.sort(key=nsize, reverse=True)
    scan.trashed += size


# ---------- comparison with the previous scan ----------

MB = 1 << 20


def history_path(root):
    base = os.environ.get("APPDATA") if WIN else os.environ.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
    if not base:
        return None
    name = hashlib.sha1(root.lower().encode("utf-8")).hexdigest()[:16] + ".json"
    return os.path.join(base, "MapaDeDisco", "history", name)


def child_dirs(d, rel, thr):
    for c in d.children:
        if isinstance(c, Dir) and c.size >= thr:
            yield c, (rel + SEP + c.name if rel else c.name)


def apply_history(scan, tree):
    """Compares the fresh tree with an earlier scan of the same folder and records a
    summary of this one. Only folder sizes are stored, never file names."""
    p = history_path(scan.root)
    if not p:
        return
    hf = {"root": scan.root, "snaps": []}
    try:
        with open(p, encoding="utf-8") as f:
            hf = json.load(f)
    except (OSError, ValueError):
        pass
    snaps = hf.get("snaps") or []
    now = int(time.time())
    base = next((s for s in reversed(snaps) if now - s["t"] >= 12 * 3600), None)
    if base is None and snaps and now - snaps[0]["t"] >= 60:
        base = snaps[0]
    if base:
        scan.prev_time = base["t"]
        dirs, thr, stack = base["dirs"], base["thr"], [(tree, "")]
        while stack:
            d, rel = stack.pop()
            if rel in dirs:
                d.delta = d.size - dirs[rel]
            elif d.size > 2 * thr:
                d.delta = d.size  # was not there before, or was too small to be recorded
            stack.extend(child_dirs(d, rel, thr // 2))
    if snaps and now - snaps[-1]["t"] < 3600:
        return  # at most one summary per hour
    thr = max(20 * MB, tree.size // 5000)
    out, stack = {}, [(tree, "")]
    while stack and len(out) < 20000:
        d, rel = stack.pop()
        out[rel] = d.size
        stack.extend(child_dirs(d, rel, thr))
    snaps.append({"t": now, "total": tree.size, "thr": thr, "dirs": out})
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w", encoding="utf-8") as f:
        json.dump({"root": scan.root, "snaps": snaps[-12:]}, f)


def collect_changes(n, path, thr, out):
    explained = False
    for c in n.children:
        if not isinstance(c, Dir):
            continue
        if abs(c.delta) >= thr or c.size >= thr:
            collect_changes(c, os.path.join(path, c.name), thr, out)
        if n.delta and (c.delta > 0) == (n.delta > 0) and abs(c.delta) * 10 >= abs(n.delta) * 8:
            explained = True  # one subfolder accounts for most of the change: show that one instead
    if abs(n.delta) >= thr and not explained:
        out.append({"p": path, "s": n.size, "g": n.delta})


# ---------- cleanup candidates ----------

CACHE_DIRS = set("node_modules|__pycache__|.cache|cache|caches|code cache|gpucache|cacheclip|media cache files|media cache|peak files|temp|tmp|.tmp|.gradle|.pytest_cache|.mypy_cache|shadercache|shader cache|crashdumps|crash reports".split("|"))
CORE_DIRS = {"windows", "program files", "program files (x86)", "programdata", "$recycle.bin", "system volume information"}
DOWNLOAD_DIRS = {"downloads", "descargas"}
TMP_EXTS = {"tmp", "temp", "dmp", "crdownload", "part"}
INSTALLER_EXTS = {"exe", "msi", "msix", "iso", "dmg", "pkg"}


def ext_of(name):
    i = name.rfind(".")
    return "" if i < 0 else name[i + 1:].lower()


def cleanup(node, path):
    groups = {k: {"total": 0, "count": 0, "items": []} for k in ("caches", "installers", "old")}
    cutoff = time.time() - 365 * 86400

    def add(k, item):
        g = groups[k]
        g["total"] += item["s"]
        g["count"] += 1
        g["items"].append(item)

    low = path.lower()
    stack = [(node, path, os.path.basename(low) in DOWNLOAD_DIRS, (SEP + "appdata") in low)]
    while stack:
        n, p0, in_dl, in_app = stack.pop()
        for c in n.children:
            if isinstance(c, Dir):
                p, ln = os.path.join(p0, c.name), c.name.lower()
                if ln in CORE_DIRS:
                    continue  # never suggest anything inside system folders
                if ln in CACHE_DIRS:
                    if c.size >= MB:
                        add("caches", {"p": p, "s": c.size, "m": c.mtime, "d": 1, "c": 0, "f": c.files})
                    continue
                stack.append((c, p, ln in DOWNLOAD_DIRS, in_app or ln == "appdata"))
                continue
            name, size, mtime, cat = c
            ext = ext_of(name)
            if ext in TMP_EXTS:
                if size >= MB:
                    add("caches", {"p": os.path.join(p0, name), "s": size, "m": mtime, "c": cat})
            elif in_dl and ext in INSTALLER_EXTS:
                add("installers", {"p": os.path.join(p0, name), "s": size, "m": mtime, "c": cat})
            elif size >= 100 * MB and 0 < mtime < cutoff and cat != 6 and not in_app:
                add("old", {"p": os.path.join(p0, name), "s": size, "m": mtime, "c": cat})
    for g in groups.values():
        g["items"].sort(key=lambda i: i["s"], reverse=True)
        del g["items"][300:]
    return groups


# ---------- duplicates ----------

class Dups:
    lock = threading.Lock()
    state, path, gen, done, total, groups = "idle", "", 0, 0, 0, []


def signature(path, size):
    """Identifies a file's content. Files up to 64 MB are hashed in full; larger ones
    by size plus 18 samples spread across the file."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        if size <= 64 * MB:
            for block in iter(lambda: f.read(MB), b""):
                h.update(block)
        else:
            for off, n in [(0, MB), (size - MB, MB)] + [(size // 17 * i, 64 << 10) for i in range(1, 17)]:
                f.seek(off)
                h.update(f.read(n))
    h.update(("|%d" % size).encode())
    return h.hexdigest()


def start_dups(path):
    with LOCK:
        node, _ = find_node(cur, path)
        if not isinstance(node, Dir):
            return False
        cands, stack = [], [(node, path)]
        while stack:
            n, p0 = stack.pop()
            for c in n.children:
                if nsize(c) < 10 * MB:
                    break  # children are sorted by size
                if isinstance(c, Dir):
                    if c.name.lower() not in CORE_DIRS:
                        stack.append((c, os.path.join(p0, c.name)))
                else:
                    cands.append((os.path.join(p0, c[0]), c[1], c[2], c[3]))
    by_size = {}
    for c in cands:
        by_size.setdefault(c[1], []).append(c)
    todo = [c for g in by_size.values() if len(g) > 1 for c in g]
    with Dups.lock:
        Dups.gen += 1
        gen = Dups.gen
        Dups.state, Dups.path, Dups.groups, Dups.done, Dups.total = "running", path, [], 0, len(todo)

    def run():
        by_sig = {}
        for c in todo:
            if Dups.gen != gen:
                return
            try:
                by_sig.setdefault(signature(c[0], c[1]), []).append(c)
            except OSError:
                pass
            Dups.done += 1
        groups = []
        for g in by_sig.values():
            if len(g) > 1:
                g.sort(key=lambda c: (c[2], c[0]))
                groups.append({"s": g[0][1], "files": [{"p": c[0], "m": c[2], "c": c[3]} for c in g]})
        groups.sort(key=lambda g: g["s"] * (len(g["files"]) - 1), reverse=True)
        with Dups.lock:
            if Dups.gen == gen:
                Dups.groups, Dups.state = groups, "done"

    threading.Thread(target=run, daemon=True).start()
    return True


# ---------- operating system ----------

def disk_space(path):
    try:
        u = shutil.disk_usage(path)
        return u.total, u.free
    except OSError:
        return 0, 0


def list_drives():
    if not WIN:
        t, f = disk_space("/")
        return [{"path": "/", "total": t, "free": f}]
    out, k32 = [], ctypes.windll.kernel32
    mask = k32.GetLogicalDrives()
    for i in range(26):
        if not mask & (1 << i):
            continue
        root = chr(65 + i) + ":\\"
        if k32.GetDriveTypeW(ctypes.c_wchar_p(root)) not in (2, 3, 4):  # removable, fixed, network
            continue
        t, f = disk_space(root)
        if t:
            out.append({"path": root, "total": t, "free": f})
    return out


def move_to_trash(path):
    """Returns None on success or a short detail string on failure."""
    if not WIN:
        d = os.path.join(os.path.expanduser("~"), ".local", "share", "Trash", "files")
        os.makedirs(d, exist_ok=True)
        try:
            os.rename(path, os.path.join(d, os.path.basename(path)))
        except OSError as e:
            return str(e.errno)
        return None
    from ctypes import wintypes

    class SHFILEOPSTRUCTW(ctypes.Structure):
        if ctypes.sizeof(ctypes.c_void_p) == 4:
            _pack_ = 1
        _fields_ = [("hwnd", wintypes.HWND), ("wFunc", wintypes.UINT), ("pFrom", wintypes.LPCWSTR),
                    ("pTo", wintypes.LPCWSTR), ("fFlags", ctypes.c_ushort), ("fAnyOperationsAborted", wintypes.BOOL),
                    ("hNameMappings", ctypes.c_void_p), ("lpszProgressTitle", wintypes.LPCWSTR)]

    buf = ctypes.create_unicode_buffer(path + "\0")  # double-null-terminated list
    op = SHFILEOPSTRUCTW()
    op.wFunc = 3  # FO_DELETE
    op.pFrom = ctypes.cast(buf, wintypes.LPCWSTR)
    # FOF_ALLOWUNDO | FOF_NOCONFIRMATION | FOF_NOERRORUI | FOF_WANTNUKEWARNING:
    # goes to the Recycle Bin; if it does not fit, Windows warns before deleting for good.
    op.fFlags = 0x40 | 0x10 | 0x400 | 0x4000
    r = ctypes.windll.shell32.SHFileOperationW(ctypes.byref(op))
    if r:
        return str(r)
    if op.fAnyOperationsAborted:
        return "cancelled"
    return None


def reveal(path):
    try:
        if WIN:
            subprocess.Popen('explorer.exe /select,"%s"' % path)
        else:
            subprocess.Popen(["xdg-open", os.path.dirname(path)])
    except OSError:
        pass


# ---------- local server ----------

TOKEN = secrets.token_hex(16)
HOSTS = set()


def load_page():
    with open(os.path.join(HERE, "web", "index.html"), encoding="utf-8") as f:
        return f.read().replace("__TOKEN__", TOKEN).replace("__VERSION__", VERSION).encode("utf-8")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def send(self, code, body, ctype="application/json; charset=utf-8"):
        if not isinstance(body, bytes):
            body = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def fail(self, code, err, detail=None):
        d = {"error": err}
        if detail:
            d["detail"] = detail
        self.send(code, d)

    def body(self):
        try:
            n = int(self.headers.get("Content-Length") or 0)
            d = json.loads(self.rfile.read(n) or b"{}")
            return d if isinstance(d, dict) else {}
        except (ValueError, OSError):
            return {}

    def allowed(self):
        return self.headers.get("Host") in HOSTS and secrets.compare_digest(self.headers.get("X-Token") or "", TOKEN)

    def do_GET(self):
        u = urlparse(self.path)
        q = {k: v[0] for k, v in parse_qs(u.query).items()}
        if u.path == "/":
            if self.headers.get("Host") not in HOSTS:
                return self.send(404, b"", "text/plain")
            return self.send(200, load_page(), "text/html; charset=utf-8")
        if not u.path.startswith("/api/"):
            return self.send(404, b"", "text/plain")
        if not self.allowed():
            return self.fail(403, "forbidden")
        fn = getattr(self, "get_" + u.path[5:], None)
        return fn(q) if fn else self.fail(404, "not_found")

    def do_POST(self):
        u = urlparse(self.path)
        if not self.allowed():
            return self.fail(403, "forbidden")
        fn = getattr(self, "post_" + u.path[5:], None) if u.path.startswith("/api/") else None
        return fn(self.body()) if fn else self.fail(404, "not_found")

    def get_places(self, q):
        folders, home = [], os.path.expanduser("~")
        if os.path.isdir(home):
            folders.append({"name": "@home", "path": home})
            for n in ("Desktop", "Documents", "Downloads", "Videos", "Pictures", "Music", "OneDrive", "AppData"):
                p = os.path.join(home, n)
                if os.path.isdir(p):
                    folders.append({"name": n, "path": p})
        self.send(200, {"drives": list_drives(), "folders": folders, "sep": SEP})

    def get_list(self, q):
        p = clean(q.get("path", ""))
        try:
            with os.scandir(p) as it:
                dirs = sorted((e.name for e in it if e.is_dir()), key=str.lower)
        except OSError:
            return self.fail(400, "unreadable")
        parent = os.path.dirname(p)
        self.send(200, {"path": p, "parent": "" if parent == p else parent, "dirs": dirs})

    def post_scan(self, b):
        global cur
        path = b.get("Path") or b.get("path")
        if not path:
            return self.fail(400, "bad_request")
        root = clean(path)
        if not os.path.isdir(root):
            return self.fail(400, "not_folder")
        with LOCK:
            if cur.state == "scanning":
                return self.fail(400, "busy")
            cur = Scan(root)
            cur.state = "scanning"
            threading.Thread(target=cur.run, daemon=True).start()
        self.send(200, {"ok": True})

    def get_status(self, q):
        with LOCK:
            s = cur
            out = {"state": s.state, "root": s.root, "files": s.files, "dirs": s.dirs, "bytes": s.bytes,
                   "errors": s.errors, "cloud": s.cloud, "elapsed": s.elapsed, "trashed": s.trashed, "sep": SEP,
                   "prevTime": s.prev_time}
            if s.state == "scanning":
                out["current"] = s.current
                out["elapsed"] = time.time() - s.started
            if s.state == "done":
                out["bytes"], out["files"] = s.tree.size, s.tree.files
        if s.root:
            out["diskTotal"], out["diskFree"] = disk_space(s.root)
        self.send(200, out)

    def get_tree(self, q):
        with LOCK:
            n, _ = find_node(cur, q.get("path", ""))
            if not isinstance(n, Dir):
                return self.fail(404, "gone")
            try:
                frac = float(q.get("frac", 0))
            except ValueError:
                frac = 0
            if not 0 < frac <= 1:
                frac = 0.0005
            t = prune(n, int(n.size * frac), 14, [15000])
            t["n"] = clean(q.get("path", ""))
            out = {"tree": t, "cats": n.cats, "root": cur.root}
        self.send(200, out)

    def get_top(self, q):
        with LOCK:
            n, _ = find_node(cur, q.get("path", ""))
            if n is None:
                return self.fail(404, "not_found")
            try:
                cat = int(q.get("cat", -1))
            except ValueError:
                cat = -1
            if not 0 <= cat < NCATS:
                cat = -1
            out = top_files(n, clean(q.get("path", "")), cat)
        self.send(200, out)

    def get_changes(self, q):
        p = clean(q.get("path", ""))
        with LOCK:
            n, _ = find_node(cur, p)
            if not isinstance(n, Dir):
                return self.fail(404, "gone")
            items = []
            if cur.prev_time:
                thr = max(50 * MB, abs(n.delta) // 200)
                for c in n.children:
                    if isinstance(c, Dir) and (abs(c.delta) >= thr or c.size >= thr):
                        collect_changes(c, os.path.join(p, c.name), thr, items)
                items.sort(key=lambda i: i["g"], reverse=True)
                if len(items) > 40:
                    items = items[:30] + items[-10:]
            out = {"prev": cur.prev_time, "delta": n.delta, "items": items}
        self.send(200, out)

    def get_cleanup(self, q):
        p = clean(q.get("path", ""))
        with LOCK:
            n, _ = find_node(cur, p)
            if not isinstance(n, Dir):
                return self.fail(404, "gone")
            out = cleanup(n, p)
        self.send(200, out)

    def post_dups(self, b):
        p = clean(b.get("Path") or b.get("path") or "")
        if not start_dups(p):
            return self.fail(400, "gone")
        self.send(200, {"ok": True})

    def get_dups(self, q):
        with Dups.lock:
            state, path, groups, done, total = Dups.state, Dups.path, Dups.groups, Dups.done, Dups.total
        out, waste = [], 0
        with LOCK:
            if path and cur.tree is not None and find_node(cur, path)[0] is None:
                state, groups = "idle", []  # belongs to an earlier scan
            for g in groups:
                files = [f for f in g["files"] if find_node(cur, f["p"])[0] is not None]
                if len(files) > 1:
                    waste += g["s"] * (len(files) - 1)
                    if len(out) < 200:
                        out.append({"s": g["s"], "files": files})
        self.send(200, {"state": state, "path": path, "done": done, "total": total, "groups": out, "waste": waste})

    def post_reveal(self, b):
        p = b.get("Path") or b.get("path") or ""
        with LOCK:
            n, _ = find_node(cur, p)
        if n is None:
            return self.fail(404, "not_found")
        reveal(clean(p))
        self.send(200, {"ok": True})

    def post_trash(self, b):
        p = clean(b.get("Path") or b.get("path") or "")
        with LOCK:
            n, chain = find_node(cur, p)
            if n is None or len(chain) < 2:
                return self.fail(400, "outside")
            err = move_to_trash(p)
            if err:
                return self.fail(500, "trash_failed", err)
            if os.path.lexists(p):
                return self.fail(500, "still_there")
            size = nsize(n)
            remove_node(cur, p)
        self.send(200, {"ok": True, "size": size})


def main():
    if not os.path.isfile(os.path.join(HERE, "web", "index.html")):
        print("No se encuentra web/index.html junto a este archivo. / web/index.html is missing next to this file.")
        sys.exit(1)
    try:
        srv = ThreadingHTTPServer(("127.0.0.1", 47600), Handler)
    except OSError:
        srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    srv.daemon_threads = True
    port = srv.server_address[1]
    HOSTS.update({"127.0.0.1:%d" % port, "localhost:%d" % port})
    url = "http://127.0.0.1:%d/" % port
    print()
    print("  Mapa de Disco %s (Python)" % VERSION)
    print("  " + url)
    print()
    print("  Deja esta ventana abierta mientras lo uses. Para salir, cierra la ventana.")
    print("  Keep this window open while you use it. Close it to quit.")
    if not os.environ.get("MAPADISCO_NO_BROWSER"):
        threading.Timer(0.4, lambda: webbrowser.open(url)).start()
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
