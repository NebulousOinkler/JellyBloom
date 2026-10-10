#!/usr/bin/env python3
"""Exercises phase 8 on the real rig: copy checks, ranking, and failover.
JellyBloom reaches the servers through testrig/faultproxy, and this script
injects faults there (a body that breaks off or stalls, an error status, an
outage, throttling, latency) and checks what a client of JellyBloom sees.
Prints PASS/FAIL per check; exits 1 on any failure.

Usage: testrig/failover.py   (after testrig/setup.py and testrig/servers.sh start)
"""
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from setup import AUTH, MEDIA, call, load_rig  # noqa: E402

FAULTS = {"a": "http://127.0.0.1:8211/__faults", "b": "http://127.0.0.1:8212/__faults"}
failures = 0


def check(name, ok, detail=""):
    global failures
    print(("PASS  " if ok else "FAIL  ") + name + ("" if ok else f": {detail}"))
    if not ok:
        failures += 1


def faults(up, **f):
    req = urllib.request.Request(FAULTS[up], data=json.dumps(f).encode(), method="POST" if f else "DELETE")
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.loads(r.read())


def counters():
    out = {}
    for up, url in FAULTS.items():
        with urllib.request.urlopen(url, timeout=10) as r:
            out[up] = json.loads(r.read())["counters"]
    return out


def served(before, after):
    """The upstreams whose media counters moved."""
    return sorted(up for up in after if after[up]["media_requests"] != before[up]["media_requests"])


def sha(data):
    return hashlib.sha256(data).hexdigest()


def file_sha(up, movie):
    with open(os.path.join(MEDIA, up, "movies", movie, movie + ".mkv"), "rb") as f:
        return sha(f.read())


class Client:
    def __init__(self, rig):
        jb = rig["jellybloom"]
        self.url = jb["url"]
        auth = call(self.url, "POST", "/Users/AuthenticateByName", {"Username": jb["username"], "Pw": jb["password"]})
        self.tok, self.uid = auth["AccessToken"], auth["User"]["Id"]

    def call(self, method, path, body=None, **query):
        return call(self.url, method, path, body, token=self.tok, query=query or None)

    def get(self, path, rng=None, timeout=120):
        """GET path (a URL path with query) as raw bytes; returns (status, headers, body, seconds)."""
        sep = "&" if "?" in path else "?"
        req = urllib.request.Request(self.url + path + sep + "api_key=" + self.tok)
        req.add_header("Authorization", AUTH + f', Token="{self.tok}"')
        if rng:
            req.add_header("Range", rng)
        start = time.time()
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.status, r.headers, r.read(), time.time() - start
        except urllib.error.HTTPError as e:
            return e.code, e.headers, e.read(), time.time() - start
        except Exception as e:  # a body that ended early
            return 0, {}, str(e).encode(), time.time() - start

    def movie(self, name):
        deadline = time.time() + 60  # the catalog loads shortly after a restart
        while True:
            items = self.call("GET", f"/Users/{self.uid}/Items", Recursive="true", IncludeItemTypes="Movie", SearchTerm=name)["Items"]
            found = [i for i in items if i["Name"] == name]
            if found or time.time() > deadline:
                return found[0]
            time.sleep(1)

    def playback_info(self, item, profile=None):
        start = time.time()
        info = self.call("POST", f"/Items/{item}/PlaybackInfo", {"DeviceProfile": profile} if profile else {}, userId=self.uid)
        return info, time.time() - start

    def stream_url(self, item, info):
        ms = info["MediaSources"][0]["Id"]
        return f"/Videos/{item}/stream.mkv?Static=true&MediaSourceId={ms}&PlaySessionId={info['PlaySessionId']}&DeviceId=failover"

    def report(self):
        return self.call("GET", "/JellyBloom/Report")


def copy_state(report, section, name, upstream):
    for it in report.get(section) or []:
        if it["Name"].startswith(name):
            for c in it["Copies"]:
                if c["Upstream"] == upstream:
                    return c["State"]
    return None


def wait_verified(jb):
    """Waits for the background verifier to check every multi-copy item."""
    deadline = time.time() + 180
    while time.time() < deadline:
        rep = jb.report()
        if rep["Totals"]["MultiCopy"] > 0 and rep["Totals"]["Unchecked"] == 0 and rep["Totals"]["Broken"] >= 2:
            return rep
        time.sleep(3)
    return jb.report()


def wait_reachable(jb, up):
    deadline = time.time() + 60
    while time.time() < deadline:
        if all(u["Reachable"] for u in jb.report()["Upstreams"] if u["Name"] == up):
            return True
        time.sleep(2)
    return False


def main():
    rig = load_rig()
    for up in FAULTS:
        faults(up)  # clear leftovers
    jb = Client(rig)

    # Copy checks: the verifier finds the damaged and truncated copies.
    rep = wait_verified(jb)
    check("report: every multi-copy item checked", rep["Totals"]["Unchecked"] == 0, rep["Totals"])
    check("report: b's Damaged Pattern is damaged", copy_state(rep, "Broken", "Damaged Pattern", "b") == "damaged",
          json.dumps(rep.get("Broken"))[:400])
    check("report: b's Truncated Pattern is truncated", copy_state(rep, "Broken", "Truncated Pattern", "b") == "truncated",
          json.dumps(rep.get("Broken"))[:400])
    check("report: a's copies are fine", copy_state(rep, "Broken", "Damaged Pattern", "a") == "ok")
    single = [i["Name"] for i in rep["SingleCopy"]]
    check("report: single-copy items listed", any(n.startswith("Colour Bars") for n in single) and
          not any(n.startswith("Long Test Pattern") for n in single), single)
    check("report: upstreams reachable", all(u["Reachable"] for u in rep["Upstreams"]), rep["Upstreams"])

    # Grouping: identical copies are one version; broken duplicates are left out.
    for name in ("Long Test Pattern", "Damaged Pattern", "Truncated Pattern"):
        info, _ = jb.playback_info(jb.movie(name)["Id"])
        names = [m.get("Name") for m in info["MediaSources"]]
        check(f"one version: {name}", len(names) == 1, names)

    # Byte-level failover: the upstream serving the session breaks off,
    # stalls, or errors; the client still gets the exact file.
    movie = "Long Test Pattern (2001)"
    want = file_sha("a", movie)
    item = jb.movie("Long Test Pattern")["Id"]
    for fault, desc in (({"cut_after": 30_000_000}, "breaks off at 30 MB"),
                        ({"stall_after": 20_000_000}, "stalls at 20 MB"),
                        ({"status": 500}, "answers 500")):
        info, _ = jb.playback_info(item)
        url = jb.stream_url(item, info)
        before = counters()
        jb.get(url, "bytes=0-1023")
        active = served(before, counters())
        if len(active) != 1:
            check(f"failover when the active upstream {desc}", False, f"could not tell the active upstream: {active}")
            continue
        faults(active[0], **fault)
        before = counters()
        status, hdrs, body, secs = jb.get(url)
        after = counters()
        faults(active[0])
        check(f"failover when the active upstream ({active[0]}) {desc}", status == 200 and sha(body) == want,
              f"HTTP {status}, {len(body)} bytes, sha match {sha(body) == want}")
        check(f"  ... the other upstream served the rest", served(before, after) == ["a", "b"] or fault.get("status"),
              served(before, after))
        check(f"  ... no per-upstream validators", "Last-Modified" not in hdrs and "ETag" not in hdrs, dict(hdrs))

    # A copy that keeps failing is quarantined; as the only copy it is still
    # tried, and serving successfully ends the quarantine.
    bars = jb.movie("Colour Bars (1971)")["Id"]
    faults("a", status=500)
    codes = []
    for _ in range(3):
        info, _ = jb.playback_info(bars)
        codes.append(jb.get(jb.stream_url(bars, info))[0])
    faults("a")
    rep = jb.report()
    check("quarantine: three failures quarantine a copy", codes == [502] * 3 and
          any(i["Name"].startswith("Colour Bars") for i in rep.get("Quarantined") or []), (codes, rep.get("Quarantined")))
    info, _ = jb.playback_info(bars)
    status = jb.get(jb.stream_url(bars, info))[0]
    rep = jb.report()
    check("quarantine: the only copy is still served, which ends the quarantine",
          status == 200 and not rep.get("Quarantined"), (status, rep.get("Quarantined")))

    # The damaged copy is still served when it is the only one (a is down).
    faults("a", down=True)
    dmg = jb.movie("Damaged Pattern")["Id"]
    info, _ = jb.playback_info(dmg)
    status, _, body, _ = jb.get(jb.stream_url(dmg, info))
    faults("a")
    check("only copy: damaged copy served while a is down",
          status == 200 and sha(body) == file_sha("b", "Damaged Pattern (2005)"), f"HTTP {status}, {len(body)} bytes")
    check("a reachable again after the outage", wait_reachable(jb, "a"))

    # HLS: the upstream transcoding a session goes down mid-play; the next
    # segments come from the other upstream's transcode.
    profile = {"MaxStreamingBitrate": 120000000, "DirectPlayProfiles": [], "CodecProfiles": [], "ContainerProfiles": [],
               "SubtitleProfiles": [], "TranscodingProfiles": [{"Container": "ts", "Type": "Video", "VideoCodec": "h264",
                                                               "AudioCodec": "aac", "Protocol": "hls", "Context": "Streaming"}]}
    info, _ = jb.playback_info(item, profile)
    tu = info["MediaSources"][0].get("TranscodingUrl", "")
    check("HLS: a transcoding URL", "master.m3u8" in tu, tu[:100])
    if "master.m3u8" in tu:
        base = tu.split("master.m3u8")[0]
        _, _, master, _ = jb.get(tu)
        variant = next((ln for ln in master.decode().splitlines() if ln and not ln.startswith("#")), "")
        _, _, main_pl, _ = jb.get(base + variant)
        segs = [ln for ln in main_pl.decode().splitlines() if ln and not ln.startswith("#")]
        before = counters()
        ok0 = [jb.get(base + s)[0] for s in segs[:2]]
        active = served(before, counters())
        check("HLS: first segments", ok0 == [200, 200] and len(active) == 1, (ok0, active))
        if len(active) == 1:
            faults(active[0], down=True)
            got = [jb.get(base + s) for s in segs[2:5]]
            faults(active[0])
            ok = all(g[0] == 200 and g[2][:1] == b"\x47" for g in got)
            check(f"HLS: segments continue after {active[0]} goes down", ok, [(g[0], len(g[2]), g[2][:20]) for g in got])
            other = "b" if active[0] == "a" else "a"
            jb.call("DELETE", "/Videos/ActiveEncodings", DeviceId="failover", PlaySessionId=info["PlaySessionId"])
            check("upstream reachable again after the HLS test", wait_reachable(jb, active[0]))

    # Speed: a throttled upstream loses plays to the faster one.
    night = jb.movie("Night of the Test Pattern")["Id"]
    info, _ = jb.playback_info(night)
    url = jb.stream_url(night, info)
    before = counters()
    jb.get(url, "bytes=0-1023")
    first = served(before, counters())
    if len(first) == 1:
        faults(first[0], throttle_kbps=1000)
        jb.get(url, "bytes=0-4194303")  # measured slow
        info, _ = jb.playback_info(night)
        before = counters()
        jb.get(jb.stream_url(night, info), "bytes=0-4194303")
        now = served(before, counters())
        faults(first[0])
        check(f"speed: after {first[0]} slows down, plays go to the other", now and first[0] not in now, (first, now))
    else:
        check("speed: could tell the serving upstream", False, first)

    # A hung upstream (accepts connections, never answers) only delays what
    # it alone can serve: first within its device session bound (10 s) plus
    # the request timeout (15 s), then within 5 s once it is known down.
    drone = next(i for i in jb.call("GET", f"/Users/{jb.uid}/Items", Recursive="true", IncludeItemTypes="Audio",
                                    SearchTerm="Drone")["Items"])["Id"]
    faults("b", latency_ms=60000)
    fast = [jb.get(p)[3] for p in (f"/UserViews?userId={jb.uid}", f"/Users/{jb.uid}/Items/{drone}", f"/Items/{bars}/Images/Primary")]
    first = jb.get(f"/Items/{drone}/Similar?userId={jb.uid}")
    second = jb.get(f"/Items/{drone}/Similar?userId={jb.uid}")
    faults("b")
    check("hung b: browsing and a's artwork stay fast", max(fast) < 1, [round(x, 2) for x in fast])
    check("hung b: a request only b can answer gives up in time", first[0] == 502 and first[3] < 28, (first[0], round(first[3], 1)))
    check("hung b: once b is known down, such requests fail fast", second[0] == 502 and second[3] < 7, (second[0], round(second[3], 1)))
    check("b reachable again after hanging", wait_reachable(jb, "b"))

    # PlaybackInfo does not wait long for a slow upstream.
    faults("b", latency_ms=8000)
    info, secs = jb.playback_info(item)
    faults("b")
    check("PlaybackInfo answers within 5 s when b is slow", secs < 5 and len(info["MediaSources"]) == 1, f"{secs:.1f} s")

    print(f"\n{failures} failure(s)" if failures else "\nall checks passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
