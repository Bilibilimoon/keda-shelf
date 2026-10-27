#!/usr/bin/env python3
"""MangaGate smoke test.

Verifies the protocol surface a client (or a future maintainer) depends on:
auth, OPDS feeds, Range streaming, covers, and admin API restrictions.

    python3 tools/smoke_test.py --base http://127.0.0.1:5080 \
        --user admin --pass test123456

Add --mutate to verify that Basic Auth accounts cannot use management APIs.
Add --gateway-admin /path/to/mangagate.sock to impersonate a NAS admin over
the fnOS gateway socket and exercise per-user series permissions, the log
endpoint and the config export. Exit code 0 means every check passed.
"""

import argparse
import base64
import http.client
import json
import os
import socket
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

NS = {"a": "http://www.w3.org/2005/Atom"}
ACQ = "http://opds-spec.org/acquisition"

passed = 0
failed = 0


def check(name, cond, detail=""):
    global passed, failed
    print(("PASS " if cond else "FAIL ") + name + (("  " + detail) if detail else ""))
    if cond:
        passed += 1
    else:
        failed += 1


class Client:
    def __init__(self, base, user, password):
        self.base = base.rstrip("/")
        self.auth = "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()

    def req(self, path, method="GET", headers=None, body=None, auth=True, password=None):
        req = urllib.request.Request(self.base + path, method=method, data=body)
        if password is not None:
            req.add_header("Authorization", "Basic " +
                           base64.b64encode(f"{self.user}:{password}".encode()).decode())
        elif auth:
            req.add_header("Authorization", self.auth)
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                return resp.status, dict(resp.headers), resp.read()
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers), e.read()


class GatewayClient:
    """Speaks to the fnOS gateway unix socket, impersonating a NAS admin."""

    def __init__(self, sock_path, name="admin"):
        self.sock_path = sock_path
        self.name = name

    def req(self, path, method="GET", body=None):
        conn = http.client.HTTPConnection("localhost", timeout=20)

        def _connect():
            s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            s.connect(self.sock_path)
            conn.sock = s

        conn.connect = _connect
        headers = {
            "X-Trim-Username": self.name,
            "X-Trim-Isadmin": "true",
            "X-Requested-With": "mangagate",
        }
        if body is not None:
            headers["Content-Type"] = "application/json"
        conn.request(method, "/app/mangagate" + path, body=body, headers=headers)
        resp = conn.getresponse()
        out = (resp.status, dict(resp.getheaders()), resp.read())
        conn.close()
        return out


def href_path(href):
    return urllib.parse.urlparse(href).path


def feed_titles(body):
    return [e.find("a:title", NS).text or "" for e in ET.fromstring(body).findall("a:entry", NS)]


class MockBangumi:
    """A tiny stand-in for api.bgm.tv, served on an ephemeral local port."""

    JPEG = b"\xff\xd8\xff\xe0" + b"\x00" * 96

    def __init__(self, port=0):
        self.last_keyword = ""
        self.port = 0
        outer = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _json(self, obj, code=200):
                body = json.dumps(obj).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def _subject(self):
                return {
                    "id": 42, "name": "Hoshi Umi Tabibito", "name_cn": "星海旅人",
                    "summary": "<p>少年在星海间旅行的物语。</p>", "date": "2020-04-01",
                    "images": {"large": f"http://127.0.0.1:{outer.port}/pic/42.jpg"},
                    "tags": [{"name": "科幻"}, {"name": "冒险"}],
                }

            def do_POST(self):
                if self.path.startswith("/v0/search/subjects"):
                    length = int(self.headers.get("Content-Length") or 0)
                    req = json.loads(self.rfile.read(length) or b"{}")
                    outer.last_keyword = req.get("keyword", "")
                    self._json({"data": [self._subject()], "total": 1})
                else:
                    self._json({"error": "not found"}, 404)

            def do_GET(self):
                if self.path == "/v0/subjects/42":
                    self._json(self._subject())
                elif self.path == "/v0/subjects/42/persons":
                    self._json([{"name": "山田花子", "relation": "作者"}])
                elif self.path == "/pic/42.jpg":
                    self.send_response(200)
                    self.send_header("Content-Type", "image/jpeg")
                    self.send_header("Content-Length", str(len(outer.JPEG)))
                    self.end_headers()
                    self.wfile.write(outer.JPEG)
                else:
                    self._json({"error": "not found"}, 404)

        self.srv = ThreadingHTTPServer(("127.0.0.1", port), H)
        self.port = self.srv.server_address[1]

    def start(self):
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def stop(self):
        self.srv.shutdown()
        self.srv.server_close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:5080")
    ap.add_argument("--user", default="admin")
    ap.add_argument("--pass", dest="password", required=True)
    ap.add_argument("--mutate", action="store_true", help="also test user create/delete")
    ap.add_argument("--gateway-admin", dest="gateway_admin", default="",
                    help="unix socket path; impersonate a NAS admin via the gateway")
    ap.add_argument("--data", default="",
                    help="runtime data dir; verifies the library cache file")
    ap.add_argument("--scrape", nargs="?", const=15800, default=None, type=int,
                    metavar="PORT",
                    help="exercise scraping against a local mock upstream on PORT "
                         "(default 15800); start the server with --scrape-base "
                         "http://127.0.0.1:PORT")
    args = ap.parse_args()

    c = Client(args.base, args.user, args.password)
    c.user = args.user

    s, _, b = c.req("/healthz", auth=False)
    check("healthz without auth", s == 200 and b"ok" in b, f"status={s}")

    s, h, _ = c.req("/opds/v1.2/catalog", auth=False)
    check("catalog rejects anonymous", s == 401, f"status={s}")

    s, _, _ = c.req("/opds/v1.2/catalog", auth=False, password="wrong-password")
    check("catalog rejects wrong password", s == 401, f"status={s}")

    s, _, b = c.req("/opds/v1.2/catalog")
    check("catalog feed", s == 200 and b"<feed" in b,
          f"status={s} type={h.get('Content-Type')}")
    if s != 200:
        print("\ncannot continue without a working catalog")
        return 1
    root = ET.fromstring(b)

    # Recent-additions feed must exist and honour permissions later on.
    s, _, b2 = c.req("/opds/v1.2/recent")
    recent_entries = len(ET.fromstring(b2).findall("a:entry", NS)) if s == 200 else 0
    check("recent feed has entries", s == 200 and recent_entries > 0,
          f"status={s} entries={recent_entries}")

    # When the catalog exposes library entries (multi-root deployments) each
    # one must be reachable.
    for e in root.findall("a:entry", NS):
        for l in e.findall("a:link", NS):
            if l.get("rel") == "subsection" and "/lib/" in (l.get("href") or ""):
                lib_path = href_path(l.get("href"))
                s2, _, _ = c.req(lib_path)
                check(f"library feed {lib_path.rsplit('/', 1)[-1]}", s2 == 200,
                      f"status={s2}")
    entries = root.findall("a:entry", NS)
    check("catalog has entries", len(entries) > 0, f"entries={len(entries)}")

    # OPDS search: OpenSearch description, a query that matches a real series
    # (title and series name fields), and queries that must return nothing.
    s, _, b = c.req("/opds/v1.2/opensearch.xml")
    check("opensearch description", s == 200 and b"OpenSearchDescription" in b,
          f"status={s}")

    first_title = ""
    for e in entries:
        title = e.findtext("a:title", default="", namespaces=NS)
        if title and title not in ("全部系列", "最近添加"):
            first_title = title
            break
    if first_title:
        s, _, b = c.req("/opds/v1.2/search?q=" + urllib.parse.quote(first_title))
        hits = len(ET.fromstring(b).findall("a:entry", NS)) if s == 200 else 0
        check("search finds a real series", s == 200 and hits > 0,
              f"status={s} hits={hits} q={first_title}")

    s, _, b = c.req("/opds/v1.2/search?q=" +
                    urllib.parse.quote("no-such-title-zzz-中文"))
    hits = len(ET.fromstring(b).findall("a:entry", NS)) if s == 200 else -1
    check("search has no false positives", s == 200 and hits == 0,
          f"status={s} hits={hits}")

    s, _, b = c.req("/opds/v1.2/search?q=")
    hits = len(ET.fromstring(b).findall("a:entry", NS)) if s == 200 else -1
    check("empty search returns nothing", s == 200 and hits == 0,
          f"status={s} hits={hits}")

    subs = [l.get("href") for e in entries for l in e.findall("a:link", NS)
            if l.get("rel") == "subsection"]
    series_paths = [u.replace(c.base, "") for u in subs if "/series/" in u]
    if not series_paths:
        print("no series to inspect (empty library)")
        return 1 if failed else 0

    acq = []
    for sp in series_paths:
        s, _, b = c.req(sp)
        if s != 200:
            continue
        for e in ET.fromstring(b).findall("a:entry", NS):
            for l in e.findall("a:link", NS):
                if l.get("rel") == ACQ:
                    acq.append((l.get("href").replace(c.base, ""), l.get("type")))
        if acq:
            break

    check("series exposes acquisition links", len(acq) > 0, f"books={len(acq)}")
    if acq:
        target, typ = acq[0]
        s, h, b = c.req(target, headers={"Range": "bytes=0-99"})
        if typ == "application/zip":
            # Image-folder books are streamed as a generated zip: no Range support.
            check("image-folder download works", s == 200 and len(b) > 0,
                  f"status={s} bytes={len(b)}")
        else:
            check("range request returns 206", s == 206 and len(b) == 100,
                  f"status={s} bytes={len(b)} range={h.get('Content-Range')}")

        s, h, b = c.req(target)
        check("book downloads", s == 200 and len(b) > 0,
              f"status={s} type={h.get('Content-Type')} bytes={len(b)}")

        thumb = target.rsplit("/", 1)[0] + "/thumbnail"
        s, h, b = c.req(thumb)
        check("cover is jpeg", s == 200 and h.get("Content-Type") == "image/jpeg"
              and b[:2] == b"\xff\xd8", f"status={s} bytes={len(b)}")

    # EPUB books must be scanned and served with the epub media type.
    epub_books = []
    for sp in series_paths:
        s, _, b = c.req(sp)
        if s != 200:
            continue
        for e in ET.fromstring(b).findall("a:entry", NS):
            for l in e.findall("a:link", NS):
                if l.get("rel") == ACQ and "epub" in (l.get("type") or ""):
                    epub_books.append(l.get("href").replace(c.base, ""))
        if epub_books:
            break
    if epub_books:
        s, h, b = c.req(epub_books[0])
        check("epub served with epub type",
              s == 200 and "epub" in (h.get("Content-Type") or "") and b[:2] == b"PK",
              f"status={s} type={h.get('Content-Type')} bytes={len(b)}")
    else:
        check("epub book present", False, "no epub acquisition link (regen testdata)")

    # Embedded epub metadata (dc:creator) flows into the feeds.
    embedded = False
    for sp in series_paths:
        s, _, b = c.req(sp)
        if s == 200 and "作者·测试".encode() in b:
            embedded = True
            break
    check("epub embedded metadata exposed", embedded)

    s, _, _ = c.req("/dav/", method="PROPFIND", headers={"Depth": "1"})
    check("webdav removed", s == 404, f"status={s}")

    s, _, b = c.req("/")
    check("admin page", s == 200 and b"<!DOCTYPE html>" in b, f"status={s}")

    s, _, b = c.req("/api/status")
    ok = False
    detail = f"status={s}"
    if s == 200:
        data = json.loads(b)
        ok = "series" in data and "opds_url" in data
        detail = f"series={data.get('series')} books={data.get('books')}"
    check("status api", ok, detail)

    s, _, _ = c.req("/api/users", auth=False, password="wrong-password")
    check("users api rejects anonymous", s in (401, 429), f"status={s}")

    if args.mutate:
        name = "smoke_reader"
        s, _, _ = c.req("/api/users/add", method="POST",
                        headers={"X-Requested-With": "mangagate",
                                 "Content-Type": "application/json"},
                        body=json.dumps({"name": name, "password": "smoke123"}).encode())
        check("basic auth cannot manage users", s == 403, f"status={s}")

    # Basic Auth accounts must never reach the admin-only read APIs either.
    for path in ("/api/series", "/api/log", "/api/export"):
        s, _, _ = c.req(path)
        check(f"basic auth blocked from {path}", s == 403, f"status={s}")

    if args.gateway_admin:
        gw = GatewayClient(args.gateway_admin)

        s, _, b = gw.req("/api/series")
        all_series = json.loads(b).get("series", []) if s == 200 else []
        check("gateway admin lists series", s == 200 and len(all_series) > 0,
              f"status={s} series={len(all_series)}")

        # Grants work on directory paths; the root-level loose series has none.
        cands = [x for x in all_series if x.get("rel") and x["rel"] != "/"]
        nested = [x for x in cands if "/" in x["rel"]]
        if len(cands) >= 2:
            allowed, hidden = cands[0], cands[1]
            s, _, _ = gw.req("/api/users/perm", "POST", json.dumps({
                "name": args.user, "restricted": True,
                "dirs": [allowed["rel"]],
            }).encode())
            check("gateway admin sets user permission", s == 200, f"status={s}")

            s, _, b = c.req("/opds/v1.2/catalog")
            titles = feed_titles(b) if s == 200 else []
            check("restricted user sees granted series",
                  allowed["name"] in titles, f"status={s} titles={titles[:5]}")
            check("restricted user hides other series",
                  hidden["name"] not in titles, f"titles={titles[:5]}")

            s, _, _ = c.req("/opds/v1.2/series/" + hidden["id"])
            check("restricted user cannot open hidden series", s == 404, f"status={s}")

            # The download endpoint must honour the same restriction.
            s, _, b = gw.req("/opds/v1.2/series/" + hidden["id"])
            hidden_file = ""
            if s == 200:
                for e in ET.fromstring(b).findall("a:entry", NS):
                    for l in e.findall("a:link", NS):
                        if l.get("rel") == ACQ:
                            hidden_file = href_path(l.get("href"))
                            break
                    if hidden_file:
                        break
            if hidden_file:
                s, _, _ = c.req(hidden_file)
                check("restricted user cannot download hidden book", s == 404,
                      f"status={s}")
            else:
                check("hidden series has a book to test", False, "no acquisition link")

            if nested:
                # A grant on the parent directory covers every series below it.
                parent = nested[0]["rel"].split("/")[0]
                below = [x for x in cands if x["rel"].startswith(parent + "/")]
                s, _, _ = gw.req("/api/users/perm", "POST", json.dumps({
                    "name": args.user, "restricted": True, "dirs": [parent],
                }).encode())
                s, _, b = c.req("/opds/v1.2/catalog")
                titles = feed_titles(b) if s == 200 else []
                ok = all(x["name"] in titles for x in below)
                check("parent grant covers nested series", ok,
                      f"parent={parent} below={[x['rel'] for x in below]} titles={titles[:10]}")

            s, _, _ = gw.req("/api/users/perm", "POST", json.dumps({
                "name": args.user, "restricted": False, "dirs": [],
            }).encode())
            s, _, b = c.req("/opds/v1.2/catalog")
            titles = feed_titles(b) if s == 200 else []
            check("permission reset restores catalog",
                  hidden["name"] in titles, f"status={s} titles={len(titles)}")
        else:
            print("(need >=2 series with rel for the permission checks; skipped)")

        s, _, b = gw.req("/api/log")
        ok = s == 200 and isinstance(json.loads(b).get("lines"), list)
        check("gateway admin reads log", ok, f"status={s}")

        s, h, b = gw.req("/api/export")
        ok = (s == 200 and b"users" in b
              and "attachment" in (h.get("Content-Disposition") or ""))
        check("gateway admin exports config", ok, f"status={s} bytes={len(b)}")

        if args.scrape:
            mock = MockBangumi(args.scrape)
            mock.start()
            try:
                s, _, b = gw.req("/api/scrape/search", "POST",
                                 json.dumps({"term": "星海旅人"}).encode())
                cands = json.loads(b).get("candidates", []) if s == 200 else []
                check("scrape search returns candidates",
                      s == 200 and len(cands) > 0 and mock.last_keyword == "星海旅人",
                      f"status={s} keyword={mock.last_keyword}")
                check("scrape candidate localized",
                      any(x.get("title") == "星海旅人" for x in cands))

                s, _, b = gw.req("/api/scrape/status")
                data = json.loads(b) if s == 200 else {}
                rows = data.get("series", [])
                terms = [x.get("term") for x in data.get("books", [])]
                check("scrape status lists cleaned terms",
                      len(rows) > 0 and "试读本" in terms,
                      f"series={len(rows)} terms={terms[:6]}")

                sid = rows[0]["id"]
                s, _, _ = gw.req("/api/scrape/apply", "POST", json.dumps({
                    "level": "series", "id": sid, "subject_id": 42}).encode())
                check("scrape apply stores match", s == 200, f"status={s}")

                s, _, b = c.req("/opds/v1.2/catalog")
                check("scraped author in catalog", "山田花子".encode() in b)
                check("scraped summary in catalog", "星海间旅行".encode() in b)

                s, _, b = gw.req("/opds/v1.2/series/" + sid + "/thumbnail")
                check("scraped cover served",
                      s == 200 and b == MockBangumi.JPEG,
                      f"status={s} bytes={len(b)}")

                s, _, _ = gw.req("/api/scrape/remove", "POST", json.dumps({
                    "level": "series", "id": sid}).encode())
                s, _, b = c.req("/opds/v1.2/catalog")
                check("scrape remove restores catalog", "山田花子".encode() not in b)

                # Batch: the mock answers every keyword with the same subject,
                # so only the series named exactly like it auto-applies and
                # everything else lands in the suggestions.
                s, _, _ = gw.req("/api/scrape/auto", "POST",
                                 json.dumps({"action": "start"}).encode())
                check("auto scrape starts", s == 200, f"status={s}")
                auto = {}
                for _ in range(90):
                    time.sleep(0.7)
                    s, _, b = gw.req("/api/scrape/status")
                    auto = json.loads(b).get("auto", {})
                    if not auto.get("running"):
                        break
                check("auto scrape finished",
                      not auto.get("running") and auto.get("done") == auto.get("total"),
                      f"done={auto.get('done')}/{auto.get('total')}")
                s, _, b = gw.req("/api/scrape/status")
                d2 = json.loads(b)
                matched = [x["name"] for x in d2["series"] if x["scraped"]]
                check("auto scrape matched the exact-name series",
                      "星海旅人" in matched, f"matched={matched}")
                sugs = d2.get("suggestions") or []
                check("auto scrape produced suggestions", len(sugs) > 0,
                      f"suggestions={len(sugs)}")
                if sugs:
                    gid = sugs[0]["series_id"]
                    s, _, _ = gw.req("/api/scrape/ignore", "POST",
                                     json.dumps({"id": gid}).encode())
                    s, _, b = gw.req("/api/scrape/status")
                    left = json.loads(b).get("suggestions") or []
                    check("ignore drops the suggestion",
                          all(x["series_id"] != gid for x in left),
                          f"left={len(left)}")

                # Export → import roundtrip.
                s, _, raw = gw.req("/api/export")
                check("export includes scraped data", s == 200 and b"scraped" in raw)
                s, _, b = gw.req("/api/import", "POST", raw)
                d3 = json.loads(b) if s == 200 else {}
                check("import roundtrip ok", s == 200 and d3.get("ok"),
                      f"status={s} added={d3.get('users_added')} "
                      f"updated={d3.get('users_updated')}")
            finally:
                mock.stop()

    if args.data:
        cache = os.path.join(args.data, "library.json")
        ok = os.path.exists(cache)
        detail = cache
        if ok:
            with open(cache) as f:
                d = json.load(f)
            ok = d.get("version") == 2 and d.get("roots") and d.get("series")
            detail = f"series={len(d.get('series') or [])} roots={d.get('roots')}"
        check("library cache written", ok, detail)

    print(f"\n{passed} passed, {failed} failed")
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
