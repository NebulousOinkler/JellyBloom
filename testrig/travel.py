#!/usr/bin/env python3
"""Runs the traveling library (scripts/travel.sh) on the rig and checks it end
to end. Server a plays home and server b plays the laptop, both reached
through the fault proxy, so "no internet" is a fault on a; the rig's main
JellyBloom (:8200) plays the home JellyBloom for sync. The library listens on
:8400, the packing list on :8401, the unpacking list on :8402.
Prints PASS/FAIL per check; exits 1 on any failure.

Usage: testrig/travel.py   (after testrig/setup.py and testrig/servers.sh start)
"""
import hashlib
import os
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from failover import Client, counters, faults, file_sha, served, sha, wait_reachable  # noqa: E402
from setup import DATA, call, load_rig  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TRAVEL = os.path.join(DATA, "travel")
TYPES = "Movie,Episode,Audio,AudioBook"
failures = 0


def check(name, ok, detail=""):
    global failures
    print(("PASS  " if ok else "FAIL  ") + name + ("" if ok else f": {detail}"))
    if not ok:
        failures += 1


def travel(*args, env=None):
    return subprocess.run([os.path.join(ROOT, "scripts", "travel.sh"), *args], env=env, capture_output=True, text=True)


def contents(base, user, pw):
    auth = call(base, "POST", "/Users/AuthenticateByName", {"Username": user, "Pw": pw})
    items = call(base, "GET", f"/Users/{auth['User']['Id']}/Items", token=auth["AccessToken"],
                 query={"Recursive": "true", "IncludeItemTypes": TYPES})["Items"]
    return {i["Name"] for i in items}


def wait_contents(base, user, pw, want):
    deadline = time.time() + 60
    got = set()
    while time.time() < deadline:
        got = contents(base, user, pw)
        if got == want:
            break
        time.sleep(1)
    return got


def at(base, rig):
    r = dict(rig)
    r["jellybloom"] = dict(rig["jellybloom"], url=base)
    return Client(r)


def main():
    rig = load_rig()
    jb = rig["jellybloom"]
    for up in ("a", "b"):
        faults(up)
    binary = os.path.join(DATA, "jellybloom")
    subprocess.run(["go", "build", "-o", binary, "./cmd/jellybloom"], cwd=ROOT, check=True)
    env = dict(os.environ, TRAVEL_DIR=TRAVEL, JELLYBLOOM=binary,
               HOME_URL="http://127.0.0.1:8211", HOME_NAME="a", LAPTOP_URL="http://127.0.0.1:8212", LAPTOP_NAME="b",
               USERS_FROM=os.path.join(DATA, "jellybloom.yaml"), HOME_JELLYBLOOM="http://127.0.0.1:8200",
               PAIRING_FROM=os.path.join(DATA, "jb", "state", "secrets"), LIBRARY_LISTEN="127.0.0.1:8400")
    travel("stop", env=env)
    r = travel("init", env=env)
    check("travel.sh init", r.returncode == 0, r.stderr[-500:])
    r = travel("start", env=env)
    check("travel.sh start", r.returncode == 0, r.stderr[-500:] + r.stdout[-300:])
    if failures:
        return 1
    try:
        with open(os.path.join(TRAVEL, "library", "config.yaml")) as f:
            cfg = f.read()
        check("config: the laptop's server is listed first and serves the web app",
              cfg.index("name: b,") < cfg.index("name: a,") and "web_upstream: b" in cfg)

        # Packing and unpacking lists.
        on = {n: contents(u["url"], u["username"], u["password"]) for n, u in rig["upstreams"].items()}
        got = wait_contents("http://127.0.0.1:8401", jb["username"], jb["password"], on["a"] - on["b"])
        check(f"packing list: exactly what home has and the laptop lacks ({len(on['a'] - on['b'])})", got == on["a"] - on["b"],
              (sorted(got), sorted(on["a"] - on["b"])))
        got = wait_contents("http://127.0.0.1:8402", jb["username"], jb["password"], on["b"] - on["a"])
        check(f"unpacking list: exactly what the laptop has and home lacks ({len(on['b'] - on['a'])})", got == on["b"] - on["a"],
              (sorted(got), sorted(on["b"] - on["a"])))

        lib = at("http://127.0.0.1:8400", rig)
        # The background check before leaving: broken copies on the laptop.
        deadline, rep = time.time() + 120, {}
        while time.time() < deadline:
            rep = lib.report()
            if rep["Totals"]["Unchecked"] == 0 and rep["Totals"]["Broken"] >= 2:
                break
            time.sleep(3)
        broken = {it["Name"]: [c["State"] for c in it["Copies"] if c["Upstream"] == "b"] for it in rep.get("Broken") or []}
        check("report: the laptop's damaged and cut-off copies are found before leaving",
              broken.get("Damaged Pattern (2005)") == ["damaged"] and broken.get("Truncated Pattern (2006)") == ["truncated"], broken)

        # Local copies play first.
        long_id = lib.movie("Long Test Pattern")["Id"]
        info, _ = lib.playback_info(long_id)
        before = counters()
        status, _, body, _ = lib.get(lib.stream_url(long_id, info), "bytes=0-1048575")
        check("a movie on both plays from the laptop", status == 206 and served(before, counters()) == ["b"],
              (status, served(before, counters())))

        # A damaged local copy is rescued from home.
        dmg = lib.movie("Damaged Pattern")["Id"]
        info, _ = lib.playback_info(dmg)
        before = counters()
        status, _, body, _ = lib.get(lib.stream_url(dmg, info))
        check("the damaged local copy is skipped: home's good copy plays",
              status == 200 and sha(body) == file_sha("a", "Damaged Pattern (2005)") and served(before, counters()) == ["a"],
              (status, served(before, counters())))

        # No internet: home is unreachable.
        faults("a", down=True)
        try:
            t0 = time.time()
            views = [v["Name"] for v in lib.call("GET", "/UserViews", userId=lib.uid)["Items"]]
            browse = time.time() - t0
            info, _ = lib.playback_info(long_id)
            status, _, _, _ = lib.get(lib.stream_url(long_id, info), "bytes=0-1023")
            web = lib.get("/web/index.html")[0]
            bars = lib.movie("Colour Bars (1971)")["Id"]
            t0 = time.time()
            home_only = lib.get(f"/Items/{bars}/PlaybackInfo?userId={lib.uid}")
            fails_in = time.time() - t0
        finally:
            faults("a")
        check("offline: browsing works from the index", browse < 1 and "Movies" in views, (browse, views))
        check("offline: packed movies play", status == 206)
        check("offline: the web app loads (served by the laptop)", web == 200, web)
        check("offline: a home-only movie fails quickly instead of hanging", home_only[0] >= 400 and fails_in < 10,
              (home_only[0], round(fails_in, 1)))
        check("home reachable again", wait_reachable(lib, "a"))

        # Watch history and playlists follow you: merge with the home JellyBloom.
        home = at("http://127.0.0.1:8200", rig)
        lib.call("POST", f"/UserPlayedItems/{long_id}")
        bars = home.movie("Colour Bars (1971)")["Id"]
        home.call("POST", f"/UserFavoriteItems/{bars}")
        pl = lib.call("POST", "/Playlists", {"Name": "Trip picks", "Ids": [long_id], "UserId": lib.uid})["Id"]
        with tempfile.NamedTemporaryFile("w", dir=TRAVEL, delete=False) as f:
            f.write(jb["password"] + "\n")
            pwfile = f.name
        os.chmod(pwfile, 0o600)
        try:
            r = travel("sync", env=dict(env, SYNC_PASSWORD_FILE=pwfile))
        finally:
            os.remove(pwfile)
        check("travel.sh sync", r.returncode == 0, r.stderr[-400:])
        ud = lambda c, i: c.call("GET", f"/Users/{c.uid}/Items/{i}")["UserData"]  # noqa: E731
        check("sync: watched on the trip shows as watched at home", ud(home, long_id)["Played"] is True)
        check("sync: favorited at home shows on the laptop", ud(lib, bars)["IsFavorite"] is True)
        pls = [p["Name"] for p in home.call("GET", f"/Users/{home.uid}/Items", IncludeItemTypes="Playlist", Recursive="true")["Items"]]
        check("sync: the trip's playlist is at home", "Trip picks" in pls, pls)
        # Undo, and sync the undo: changes and deletions propagate too.
        lib.call("DELETE", f"/UserPlayedItems/{long_id}")
        home.call("DELETE", f"/UserFavoriteItems/{bars}")
        lib.call("DELETE", f"/Items/{pl}")
        with tempfile.NamedTemporaryFile("w", dir=TRAVEL, delete=False) as f:
            f.write(jb["password"] + "\n")
            pwfile = f.name
        try:
            travel("sync", env=dict(env, SYNC_PASSWORD_FILE=pwfile))
        finally:
            os.remove(pwfile)
        pls = [p["Name"] for p in home.call("GET", f"/Users/{home.uid}/Items", IncludeItemTypes="Playlist", Recursive="true")["Items"]]
        check("sync: undoing propagates (unwatched, unfavorited, playlist deleted)",
              ud(home, long_id)["Played"] is False and ud(lib, bars)["IsFavorite"] is False and "Trip picks" not in pls,
              (ud(home, long_id)["Played"], ud(lib, bars)["IsFavorite"], pls))
    finally:
        r = travel("stop", env=env)
        check("travel.sh stop", r.returncode == 0, r.stderr[-300:])
    print(f"\n{failures} failure(s)" if failures else "\nall checks passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
