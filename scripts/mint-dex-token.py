#!/usr/bin/env python3
"""mint-dex-token.py — mint id_token từ Dex dev (host-side CLI, stdlib only).

Port Python của tests/integration/dex.go (nguồn gốc: opsense-rs common/dex.rs).
Dùng cho debug/manual test: curl -H "Authorization: Bearer $(python3 scripts/mint-dex-token.py)" ...

Env override: LTP_DEX_ISSUER (default http://localhost:5556/dex),
LTP_DEX_CLIENT_ID, LTP_DEX_CLIENT_SECRET, LTP_DEX_USER, LTP_DEX_PASSWORD.
"""
import os
import re
import sys
import http.cookiejar
import urllib.error
import urllib.parse
import urllib.request

ISSUER = os.environ.get("LTP_DEX_ISSUER", "http://localhost:5556/dex")
CLIENT_ID = os.environ.get("LTP_DEX_CLIENT_ID", "ltp-test")
CLIENT_SECRET = os.environ.get("LTP_DEX_CLIENT_SECRET", "ltp-dev-shared-secret-32-bytes-min!!")
USER = os.environ.get("LTP_DEX_USER", "dev-user@example.com")
PASSWORD = os.environ.get("LTP_DEX_PASSWORD", "password")
REDIRECT_URI = "http://localhost:8080/callback"  # gateway /login dựng http://<host>/callback (dev HTTP)

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


opener = urllib.request.build_opener(
    urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar())
)

FORM_RE = re.compile(r"(?is)<form[^>]*action=\"([^\"]+)\"")
INPUT_RE = re.compile(r"(?is)<input\b[^>]*>")
ATTR_RE = lambda a: re.compile(r"(?is)\b" + a + r"=\"([^\"]*)\"")


def get(url):
    try:
        with opener.open(url) as r:
            return r.status, dict(r.headers), r.read().decode()
    except urllib.error.HTTPError as e:
        # Dex trả 302 redirect (chưa có session → form login, hoặc redirect về
        # callback sau khi POST credentials). Không raise: main() đọc header
        # Location và tự theo dõi redirect.
        if e.code in (301, 302, 303, 307, 308) and e.headers.get("Location"):
            return e.code, dict(e.headers), ""
        raise


def post(url, data):
    body = urllib.parse.urlencode(data).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/x-www-form-urlencoded"})
    try:
        with opener.open(req) as r:
            return r.status, dict(r.headers), r.read().decode()
    except urllib.error.HTTPError as e:
        if e.code in (301, 302, 303, 307, 308) and e.headers.get("Location"):
            return e.code, dict(e.headers), ""
        raise


def parse_form(html):
    m = FORM_RE.search(html)
    if not m:
        return None
    action = m.group(1).replace("&amp;", "&")
    fields = {}
    for tag in INPUT_RE.findall(html):
        t = ATTR_RE("type").search(tag)
        n = ATTR_RE("name").search(tag)
        if not n or (t and t.group(1) != "hidden"):
            continue
        v = ATTR_RE("value").search(tag)
        fields[n.group(1)] = v.group(1) if v else ""
    if "password" in html and "login" not in fields:
        fields["login"] = USER
        fields["password"] = PASSWORD
    return action, fields


def resolve(current, loc):
    return urllib.parse.urljoin(current, loc)


def main():
    disc = ISSUER + "/.well-known/openid-configuration"
    st, _, body = get(disc)
    ep = __import__("json").loads(body)
    authz = (ep["authorization_endpoint"] + "?" + urllib.parse.urlencode({
        "response_type": "code", "client_id": CLIENT_ID, "redirect_uri": REDIRECT_URI,
        "scope": "openid email profile offline_access"}))

    current, form = authz, None
    code = None
    for _ in range(10):
        if form:
            st, hdrs, body = post(resolve(current, form[0]), form[1])
        else:
            st, hdrs, body = get(current)
        loc = hdrs.get("Location") or hdrs.get("location")
        if st in (301, 302, 303, 307, 308) and loc:
            q = urllib.parse.parse_qs(urllib.parse.urlparse(resolve(current, loc)).query)
            if q.get("code"):
                code = q["code"][0]
                break
            current, form = resolve(current, loc), None
            continue
        form = parse_form(body)
        if not form:
            sys.exit(f"unexpected page at {current} (status {st})")

    if not code:
        sys.exit("no authorization code captured")

    st, _, body = post(ep["token_endpoint"], {
        "grant_type": "authorization_code", "code": code, "redirect_uri": REDIRECT_URI,
        "client_id": CLIENT_ID, "client_secret": CLIENT_SECRET})
    if st != 200:
        sys.exit(f"token exchange failed: {st} {body[:200]}")
    print(__import__("json").loads(body)["id_token"])


if __name__ == "__main__":
    main()
