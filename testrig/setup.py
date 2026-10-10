#!/usr/bin/env python3
"""Configures the two local test Jellyfin servers and writes a JellyBloom config.

Completes each server's startup wizard, creates its libraries, waits for the
library scan, then writes testrig/data/jellybloom.yaml. Test credentials are
generated once and kept in testrig/data/rig.json (git-ignored).

Usage: testrig/setup.py   (servers must be running: testrig/servers.sh start)
"""
import json
import os
import re
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(ROOT, "data")
MEDIA = os.path.join(DATA, "media")
RIG = os.path.join(DATA, "rig.json")

# JellyBloom reaches each server through testrig/faultproxy ("via"), which
# passes everything through until a test injects faults.
SERVERS = {
    "a": {"url": "http://127.0.0.1:8201", "via": "http://127.0.0.1:8211",
          "libraries": [("Movies", "movies", "movies"), ("Shows", "tvshows", "shows")]},
    "b": {"url": "http://127.0.0.1:8202", "via": "http://127.0.0.1:8212",
          "libraries": [("Music", "music", "music"), ("Movies", "movies", "movies"), ("Books", "books", "books")]},
}
AUTH = 'MediaBrowser Client="testrig", Device="setup", DeviceId="testrig-setup", Version="1"'
NO_FETCHERS = [  # keep scans offline: no internet metadata (local image extraction stays on)
    {"Type": t, "MetadataFetchers": [], "MetadataFetcherOrder": []}
    for t in ["Movie", "Series", "Season", "Episode", "MusicAlbum", "MusicArtist", "Audio", "AudioBook", "Book"]
]
# Upstream collections (Jellyfin BoxSets), created after the scan: name -> member movies.
COLLECTIONS = {"a": {"Test Pattern Collection": ["Colour Bars (1971)", "Long Test Pattern"]}}


def call(base, method, path, body=None, token=None, query=None):
    url = base + path + ("?" + urllib.parse.urlencode(query, doseq=True) if query else "")
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", AUTH + (f', Token="{token}"' if token else ""))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            raw = r.read()
            return json.loads(raw) if raw.strip() else None
    except urllib.error.HTTPError as e:
        raise SystemExit(f"{method} {url}: HTTP {e.code}: {e.read()[:300]!r}")


def save_rig(rig):
    os.makedirs(DATA, exist_ok=True)
    with open(RIG, "w") as f:
        json.dump(rig, f, indent=2)
    os.chmod(RIG, 0o600)


def load_rig():
    if os.path.exists(RIG):
        with open(RIG) as f:
            rig = json.load(f)
        changed = False
        for u in rig["upstreams"].values():  # added after the first rig version
            if "service" not in u:
                u["service"] = {"username": "jellybloom", "password": secrets.token_urlsafe(12)}
                changed = True
        if changed:
            save_rig(rig)
        return rig
    rig = {
        # rigadmin sets the servers up; JellyBloom pairs as the non-admin "jellybloom" account.
        "upstreams": {n: {"url": s["url"], "username": "rigadmin", "password": secrets.token_urlsafe(12),
                          "service": {"username": "jellybloom", "password": secrets.token_urlsafe(12)}} for n, s in SERVERS.items()},
        "jellybloom": {"url": "http://127.0.0.1:8200", "username": "tester", "password": secrets.token_urlsafe(12)},
    }
    save_rig(rig)
    return rig


def item_count(base, token):
    r = call(base, "GET", "/Items", token=token, query={"Recursive": "true", "Limit": "0"})
    return r["TotalRecordCount"]


def setup_server(name, spec, creds):
    base = spec["url"]
    info = call(base, "GET", "/System/Info/Public")
    if not info.get("StartupWizardCompleted"):
        call(base, "POST", "/Startup/Configuration", {"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"})
        call(base, "GET", "/Startup/User")
        call(base, "POST", "/Startup/User", {"Name": creds["username"], "Password": creds["password"]})
        call(base, "POST", "/Startup/RemoteAccess", {"EnableRemoteAccess": True, "EnableAutomaticPortMapping": False})
        call(base, "POST", "/Startup/Complete")
        print(f"{name}: startup wizard completed")
    auth = call(base, "POST", "/Users/AuthenticateByName", {"Username": creds["username"], "Pw": creds["password"]})
    token = auth["AccessToken"]

    svc = creds["service"]
    if svc["username"] not in {u["Name"] for u in call(base, "GET", "/Users", token=token)}:
        call(base, "POST", "/Users/New", {"Name": svc["username"], "Password": svc["password"]}, token=token)
        print(f"{name}: created non-admin account {svc['username']} for JellyBloom")

    existing = {f["Name"] for f in call(base, "GET", "/Library/VirtualFolders", token=token)}
    for lib, ctype, folder in spec["libraries"]:
        if lib in existing:
            continue
        path = os.path.join(MEDIA, name, folder)
        call(base, "POST", "/Library/VirtualFolders", {"LibraryOptions": {"TypeOptions": NO_FETCHERS, "EnableRealtimeMonitor": False}},
             token=token, query={"name": lib, "collectionType": ctype, "paths": path, "refreshLibrary": "false"})
        print(f"{name}: created library {lib} -> {path}")
    call(base, "POST", "/Library/Refresh", token=token)

    # Wait for the scan to settle: the item count stops changing.
    last, stable = -1, 0
    for _ in range(90):
        time.sleep(2)
        n = item_count(base, token)
        stable = stable + 1 if n == last and n > 0 else 0
        last = n
        if stable >= 3:
            break
    print(f"{name}: scan settled at {last} items")

    have = {b["Name"] for b in call(base, "GET", "/Items", token=token, query={"Recursive": "true", "IncludeItemTypes": "BoxSet"})["Items"]}
    for cname, members in COLLECTIONS.get(name, {}).items():
        if cname in have:
            continue
        movies = call(base, "GET", "/Items", token=token, query={"Recursive": "true", "IncludeItemTypes": "Movie"})["Items"]
        ids = [m["Id"] for m in movies if m["Name"] in members]
        call(base, "POST", "/Collections", token=token, query={"name": cname, "ids": ",".join(ids)})
        print(f"{name}: created collection {cname}")


def write_jellybloom_config(rig):
    binary = os.path.join(DATA, "jellybloom")
    subprocess.run(["go", "build", "-o", binary, "./cmd/jellybloom"], cwd=os.path.dirname(ROOT), check=True)
    jb = rig["jellybloom"]
    h = subprocess.run([binary, "hash-password"], input=jb["password"] + "\n", capture_output=True, text=True, check=True).stdout.strip()
    ups = "\n".join(f'  - {{name: {n}, url: "{SERVERS[n]["via"]}"}}' for n in rig["upstreams"])
    cfg = f"""# Generated by testrig/setup.py. No upstream secrets: upstreams are paired
# with `jellybloom pair` and their tokens live in data_dir/secrets.
listen: "127.0.0.1:8200"
server_name: "JellyBloom Rig"
data_dir: "{os.path.join(DATA, 'jb', 'state')}"
merge: union
web_upstream: a
stall_timeout: 3s
upstreams:
{ups}
users:
  - {{name: "{jb['username']}", password_hash: "{h}", admin: true}}
"""
    path = os.path.join(DATA, "jellybloom.yaml")
    with open(path, "w") as f:
        f.write(cfg)
    os.chmod(path, 0o600)
    print(f"wrote {path} and {binary}")
    return binary, path


def pair(binary, config_path, name, rig):
    """Pairs an upstream through `jellybloom pair`, approving the Quick Connect
    code as the non-admin service account (what a person does in their app)."""
    secret = os.path.join(DATA, "jb", "state", "secrets", f"upstream-{name}")
    if os.path.exists(secret):
        return
    base, svc = rig["upstreams"][name]["url"], rig["upstreams"][name]["service"]
    proc = subprocess.Popen([binary, "pair", "-config", config_path, name], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    code = None
    for line in proc.stdout:
        m = re.search(r"enter this code:\s+(\d+)", line)
        if m:
            code = m.group(1)
            auth = call(base, "POST", "/Users/AuthenticateByName", {"Username": svc["username"], "Pw": svc["password"]})
            call(base, "POST", "/QuickConnect/Authorize", token=auth["AccessToken"], query={"code": code})
            call(base, "POST", "/Sessions/Logout", token=auth["AccessToken"])
        elif line.startswith("Paired") or line.startswith("error"):
            print(f"{name}: {line.strip()}")
    if proc.wait() != 0 or code is None:
        raise SystemExit(f"pairing {name} failed")


def main():
    rig = load_rig()
    for name, spec in SERVERS.items():
        setup_server(name, spec, rig["upstreams"][name])
    binary, config_path = write_jellybloom_config(rig)
    for name in SERVERS:
        pair(binary, config_path, name, rig)
    print("credentials: testrig/data/rig.json")


if __name__ == "__main__":
    sys.exit(main())
