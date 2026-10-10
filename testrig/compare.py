#!/usr/bin/env python3
"""Runs the same user-state steps on a real Jellyfin server and on JellyBloom
and compares the results field by field: playback reports (start, progress,
stop) for a long movie, a long music track, and an audiobook; marking played
and unplayed; Continue Watching; and Next Up in its variants.

Real Jellyfin is the reference: any difference is a JellyBloom bug. Uses the
rig's admin account on the upstreams and the rig user on JellyBloom, and
changes their watch state. Prints each difference; exits 1 if there are any.

Usage: testrig/compare.py   (after testrig/setup.py and testrig/servers.sh start)
"""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from setup import call, load_rig  # noqa: E402


class Server:
    def __init__(self, url, user, pw):
        self.url = url
        auth = call(url, "POST", "/Users/AuthenticateByName", {"Username": user, "Pw": pw})
        self.tok, self.uid = auth["AccessToken"], auth["User"]["Id"]

    def call(self, method, path, body=None, **query):
        return call(self.url, method, path, body, token=self.tok, query=query or None)

    def find(self, prefix, typ):
        items = self.call("GET", f"/Users/{self.uid}/Items", Recursive="true", IncludeItemTypes=typ, SearchTerm=prefix)["Items"]
        return next(i for i in items if i["Name"].startswith(prefix))

    def userdata(self, item):
        d = self.call("GET", f"/Users/{self.uid}/Items/{item}")["UserData"]
        return {k: d.get(k) for k in ("Played", "PlayCount", "PlaybackPositionTicks", "IsFavorite", "Likes", "LastPlayedDate", "PlayedPercentage")}


# Playback scenarios: (item name, type, upstream holding it, steps). A step is
# (label, action, fraction of the runtime or None).
PLAYBACK = [
    ("Long Test Pattern", "Movie", "a", [
        ("reset", "reset", None), ("start at 50%", "start", 0.5), ("progress 3%", "progress", 0.03),
        ("progress 50%", "progress", 0.5), ("stop 50%", "stop", 0.5), ("start again", "start", None),
        ("progress 95%", "progress", 0.95), ("stop 95%", "stop", 0.95), ("start a played item", "start", 0),
        ("stop 2%", "stop", 0.02), ("mark played with a date", "played_date", None), ("mark played", "played", None),
        ("mark unplayed", "unplayed", None), ("stop without a position", "stop", None)]),
    ("Drone", "Audio", "b", [
        ("reset", "reset", None), ("start", "start", 0), ("progress 50%", "progress", 0.5), ("stop 50%", "stop", 0.5),
        ("mark unplayed", "unplayed", None), ("start", "start", 0), ("stop 2%", "stop", 0.02)]),
    ("The Long Tone", "AudioBook", "b", [
        ("reset", "reset", None), ("start", "start", 0), ("progress 2 min", "progress", 120 / 720),
        ("progress 6 min", "progress", 360 / 720), ("stop 6 min", "stop", 360 / 720), ("start again", "start", None),
        ("stop with 2 min left", "stop", 600 / 720), ("mark unplayed", "unplayed", None), ("stop 4%", "stop", 0.04),
        ("stop without a position", "stop", None)]),
]


def run_playback(srv, item, steps):
    iid, rt = item["Id"], item["RunTimeTicks"]
    out, prev = [], None
    for label, action, frac in steps:
        time.sleep(1.1)  # so "LastPlayedDate moved" is observable
        body = {"ItemId": iid}
        if frac is not None:
            body["PositionTicks"] = int(rt * frac)
        if action == "reset":
            for path in (f"/UserPlayedItems/{iid}", f"/UserFavoriteItems/{iid}", f"/UserItems/{iid}/Rating"):
                srv.call("DELETE", path)
        elif action == "unplayed":
            srv.call("DELETE", f"/UserPlayedItems/{iid}")
        elif action == "played":
            srv.call("POST", f"/UserPlayedItems/{iid}")
        elif action == "played_date":
            srv.call("POST", f"/UserPlayedItems/{iid}", datePlayed="2020-01-02T03:04:05.000Z")
        else:
            srv.call("POST", {"start": "/Sessions/Playing", "progress": "/Sessions/Playing/Progress",
                              "stop": "/Sessions/Playing/Stopped"}[action], body)
        d = srv.userdata(iid)
        lp = d.get("LastPlayedDate")
        d["LastPlayedDate"] = "none" if not lp else lp[:19] if lp.startswith("2020") else "same" if lp == prev else "moved"
        prev = lp
        d["PlaybackPositionTicks"] = round((d.get("PlaybackPositionTicks") or 0) / rt, 3)
        if d.get("PlayedPercentage") is not None:
            d["PlayedPercentage"] = round(d["PlayedPercentage"])
        out.append((label, {k: v for k, v in d.items() if v is not None}))
    return out


def rewatch_resume(srv):
    """A played movie being watched again is in Continue Watching."""
    m = srv.find("Long Test Pattern", "Movie")
    iid, rt = m["Id"], m["RunTimeTicks"]
    srv.call("DELETE", f"/UserPlayedItems/{iid}")
    srv.call("POST", f"/UserPlayedItems/{iid}")
    srv.call("POST", "/Sessions/Playing", {"ItemId": iid})
    srv.call("POST", "/Sessions/Playing/Progress", {"ItemId": iid, "PositionTicks": rt // 2})
    resume = "Long Test Pattern" in [i["Name"] for i in srv.call("GET", f"/Users/{srv.uid}/Items/Resume", MediaTypes="Video")["Items"]]
    srv.call("POST", "/Sessions/Playing/Stopped", {"ItemId": iid, "PositionTicks": rt // 2})
    srv.call("DELETE", f"/UserPlayedItems/{iid}")
    return {"in Continue Watching": resume}


def day(d):
    return f"2026-10-{d:02d}T12:00:00.000Z"


# Next Up scenarios over Test Show (S1E1-4, S2E1): (name, [(episode, played day
# or 0 for in progress)], query parameters).
NEXT_UP = [
    ("one played", [("S1E1", 1)], {}),
    ("gap: E1, E2, E4 played", [("S1E1", 1), ("S1E2", 2), ("S1E4", 3)], {}),
    ("rewatched an early episode last", [("S1E1", 1), ("S1E2", 2), ("S1E4", 3), ("S1E1", 5)], {}),
    ("same, enableRewatching", [("S1E1", 1), ("S1E2", 2), ("S1E4", 3), ("S1E1", 5)], {"enableRewatching": "true"}),
    ("rewatching, resumable off", [("S1E4", 1), ("S1E1", 2)], {"enableRewatching": "true", "enableResumable": "false"}),
    ("all played, rewatching", [("S1E1", 1), ("S1E2", 2), ("S1E3", 3), ("S1E4", 4), ("S2E1", 5)], {"enableRewatching": "true"}),
    ("all played", [("S1E1", 1), ("S1E2", 2), ("S1E3", 3), ("S1E4", 4), ("S2E1", 5)], {}),
    ("next in progress", [("S1E1", 1), ("S1E2", 0)], {}),
    ("next in progress, resumable off", [("S1E1", 1), ("S1E2", 0)], {"enableResumable": "false"}),
    ("last watched before the cutoff", [("S1E1", 1)], {"nextUpDateCutoff": "2026-10-05T00:00:00.000Z"}),
    ("nothing watched", [], {}),
    ("nothing watched, by series", [], {"seriesId": True}),
    ("nothing watched, by series, disableFirstEpisode", [], {"seriesId": True, "disableFirstEpisode": "true"}),
]


def next_up(srv, steps, params):
    series = srv.find("Test Show", "Series")["Id"]
    eps = {f"S{e['ParentIndexNumber']}E{e['IndexNumber']}": e for e in srv.call("GET", f"/Shows/{series}/Episodes", userId=srv.uid)["Items"]}
    for e in eps.values():
        srv.call("DELETE", f"/UserPlayedItems/{e['Id']}")
    for ep, d in steps:
        e = eps[ep]
        if d:
            srv.call("POST", f"/UserPlayedItems/{e['Id']}", datePlayed=day(d))
        else:
            srv.call("POST", f"/UserItems/{e['Id']}/UserData", {"PlaybackPositionTicks": e["RunTimeTicks"] // 2,
                                                                 "LastPlayedDate": day(4)})
    q = dict(params, userId=srv.uid)
    if q.get("seriesId"):
        q["seriesId"] = series
    got = [i["Name"].removeprefix("Test Show ") for i in srv.call("GET", "/Shows/NextUp", **q)["Items"]]
    for e in eps.values():
        srv.call("DELETE", f"/UserPlayedItems/{e['Id']}")
    return got


def main():
    rig = load_rig()
    ups = {n: Server(u["url"], u["username"], u["password"]) for n, u in rig["upstreams"].items()}
    jb = Server(rig["jellybloom"]["url"], rig["jellybloom"]["username"], rig["jellybloom"]["password"])
    diffs = 0

    def compare(label, real, ours):
        nonlocal diffs
        if real == ours:
            print(f"same  {label}: {real}")
        else:
            diffs += 1
            print(f"DIFF  {label}\n        jellyfin:   {real}\n        jellybloom: {ours}")

    for name, typ, up, steps in PLAYBACK:
        real = run_playback(ups[up], ups[up].find(name, typ), steps)
        ours = run_playback(jb, jb.find(name, typ), steps)
        for (label, r), (_, o) in zip(real, ours):
            compare(f"{name}: {label}", r, o)
    compare("rewatching a played movie", rewatch_resume(ups["a"]), rewatch_resume(jb))
    for name, steps, params in NEXT_UP:
        compare(f"next up: {name}", next_up(ups["a"], steps, params), next_up(jb, steps, params))

    print(f"\n{diffs} difference(s) from real Jellyfin" if diffs else "\nsame as real Jellyfin")
    return 1 if diffs else 0


if __name__ == "__main__":
    sys.exit(main())
