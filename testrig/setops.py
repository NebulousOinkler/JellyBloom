#!/usr/bin/env python3
"""Runs JellyBloom in each set operation against the rig's servers and checks
what a client sees against the servers' own contents: intersection on :8301,
a \\ b on :8302, b \\ a on :8303. Each instance gets its own data directory and
a copy of the main instance's upstream pairing, and is stopped afterwards.
Prints PASS/FAIL per check; exits 1 on any failure.

Usage: testrig/setops.py   (after testrig/setup.py and testrig/servers.sh start)
"""
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from setup import AUTH, DATA, SERVERS, call, load_rig  # noqa: E402

TYPES = "Movie,Episode,Audio,AudioBook"
MODES = [
    ("intersection", 8301, "merge: intersection"),
    ("a minus b", 8302, "merge: difference\nbase: a\nsubtract: [b]"),
    ("b minus a", 8303, "merge: difference\nbase: b\nsubtract: [a]"),
]
failures = 0


def check(name, ok, detail=""):
    global failures
    print(("PASS  " if ok else "FAIL  ") + name + ("" if ok else f": {detail}"))
    if not ok:
        failures += 1


def contents(base, user, pw):
    """Names of playable items on a server (or a JellyBloom instance)."""
    auth = call(base, "POST", "/Users/AuthenticateByName", {"Username": user, "Pw": pw})
    items = call(base, "GET", f"/Users/{auth['User']['Id']}/Items", token=auth["AccessToken"],
                 query={"Recursive": "true", "IncludeItemTypes": TYPES})["Items"]
    return {i["Name"] for i in items}, auth


def start(name, port, merge, rig):
    state = os.path.join(DATA, "setops", f"{port}")
    shutil.rmtree(state, ignore_errors=True)
    os.makedirs(os.path.join(state, "secrets"), mode=0o700)
    src = os.path.join(DATA, "jb", "state", "secrets")
    for f in os.listdir(src):  # the main instance's pairing
        if f.startswith("upstream-"):
            shutil.copy2(os.path.join(src, f), os.path.join(state, "secrets", f))
    with open(os.path.join(DATA, "jellybloom.yaml")) as f:
        main_cfg = f.read()
    users = main_cfg[main_cfg.index("users:"):]
    ups = "\n".join(f'  - {{name: {n}, url: "{SERVERS[n]["via"]}"}}' for n in rig["upstreams"])
    cfg = os.path.join(state, "config.yaml")
    with open(cfg, "w") as f:
        f.write(f'listen: "127.0.0.1:{port}"\nserver_name: "JellyBloom {name}"\ndata_dir: "{state}"\n'
                f'{merge}\nweb_upstream: a\nupstreams:\n{ups}\n{users}')
    os.chmod(cfg, 0o600)
    log = open(os.path.join(state, "jellybloom.log"), "w")
    return subprocess.Popen([os.path.join(DATA, "jellybloom"), "-config", cfg], stdout=log, stderr=subprocess.STDOUT)


def wait_ready(base, user, pw, want):
    """Waits until the instance has synced (its contents stop changing)."""
    deadline, last, stable = time.time() + 90, None, 0
    while time.time() < deadline:
        try:
            got, _ = contents(base, user, pw)
            stable = stable + 1 if got == last and (got or not want) else 0
            last = got
            if stable >= 3:
                return got
        except (urllib.error.URLError, SystemExit, ConnectionError):
            pass
        time.sleep(1)
    return last or set()


def first_bytes(base, tok, item, ms):
    req = urllib.request.Request(f"{base}/Videos/{item}/stream?Static=true&MediaSourceId={ms}&api_key={tok}")
    req.add_header("Authorization", AUTH + f', Token="{tok}"')
    req.add_header("Range", "bytes=0-1023")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, len(r.read())
    except urllib.error.HTTPError as e:
        return e.code, 0


def main():
    rig = load_rig()
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    subprocess.run(["go", "build", "-o", os.path.join(DATA, "jellybloom"), "./cmd/jellybloom"], cwd=root, check=True)
    on = {n: contents(u["url"], u["username"], u["password"])[0] for n, u in rig["upstreams"].items()}
    expect = {"intersection": on["a"] & on["b"], "a minus b": on["a"] - on["b"], "b minus a": on["b"] - on["a"]}
    jb = rig["jellybloom"]
    procs = [(name, port, start(name, port, merge, rig)) for name, port, merge in MODES]
    try:
        for name, port, proc in procs:
            base = f"http://127.0.0.1:{port}"
            got = wait_ready(base, jb["username"], jb["password"], expect[name])
            check(f"{name}: exactly the expected items ({len(expect[name])})", got == expect[name],
                  f"missing {sorted(expect[name] - got)}, extra {sorted(got - expect[name])}")
            _, auth = contents(base, jb["username"], jb["password"])
            tok, uid = auth["AccessToken"], auth["User"]["Id"]
            libs = call(base, "GET", "/UserViews", token=tok, query={"userId": uid})["Items"]
            for lib in libs:
                n = call(base, "GET", f"/Users/{uid}/Items", token=tok, query={"ParentId": lib["Id"], "Limit": "1"})["TotalRecordCount"]
                check(f"{name}: library {lib['Name']} is not empty", n > 0)
            # A visible movie plays, from a copy that takes part.
            movies = call(base, "GET", f"/Users/{uid}/Items", token=tok,
                          query={"Recursive": "true", "IncludeItemTypes": "Movie"})["Items"]
            if movies:
                m = movies[0]
                info = call(base, "POST", f"/Items/{m['Id']}/PlaybackInfo", {}, token=tok, query={"userId": uid})
                status, n = first_bytes(base, tok, m["Id"], info["MediaSources"][0]["Id"])
                check(f"{name}: {m['Name']} plays", status == 206 and n == 1024, (status, n))
            # Hidden items are not reachable by search.
            hidden = (on["a"] | on["b"]) - expect[name]
            if hidden:
                h = sorted(hidden)[0]
                hits = call(base, "GET", "/Search/Hints", token=tok, query={"searchTerm": h, "userId": uid})["SearchHints"]
                check(f"{name}: hidden {h!r} not found by search", all(x["Name"] != h for x in hits), hits)
    finally:
        for _, _, proc in procs:
            proc.terminate()
        for _, _, proc in procs:
            proc.wait(timeout=20)
    print(f"\n{failures} failure(s)" if failures else "\nall checks passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
