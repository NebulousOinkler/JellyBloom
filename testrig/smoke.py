#!/usr/bin/env python3
"""Drives JellyBloom like a Jellyfin client against the local test rig.

Checks login, merged libraries, browsing, details, artwork, PlaybackInfo
versions, HLS, direct play, and music, and that no upstream IDs, tokens, or
addresses reach the client. Prints PASS/FAIL per check; exits 1 on failure.

Usage: testrig/smoke.py   (after testrig/servers.sh start)
"""
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from setup import SERVERS, call, load_rig  # noqa: E402

JB = "http://127.0.0.1:8200"
HDR = 'MediaBrowser Client="smoke", Device="smoke-test", DeviceId="smoke-device-1", Version="1"'
failures = []


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else f"  -- {detail}"))
    if not ok:
        failures.append(name)


def req(path, token=None, method="GET", body=None, headers=None, raw=False):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(JB + path, data=data, method=method)
    r.add_header("Authorization", HDR + (f', Token="{token}"' if token else ""))
    if data is not None:
        r.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        r.add_header(k, v)
    try:
        with urllib.request.urlopen(r, timeout=60) as resp:
            b = resp.read()
            if raw:
                return resp.status, resp.headers, b
            return resp.status, resp.headers, (json.loads(b) if b.strip() else None)
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def upstream_secrets(rig):
    """Upstream IDs, tokens, and addresses that must never reach a client."""
    leaks = set()
    for name, spec in SERVERS.items():
        c = rig["upstreams"][name]
        auth = call(spec["url"], "POST", "/Users/AuthenticateByName", {"Username": c["username"], "Pw": c["password"]})
        tok = auth["AccessToken"]
        leaks |= {spec["url"], auth["User"]["Id"], call(spec["url"], "GET", "/System/Info/Public")["Id"]}
        for it in call(spec["url"], "GET", "/Items", token=tok, query={"Recursive": "true"})["Items"]:
            leaks.add(it["Id"])
        for v in call(spec["url"], "GET", "/UserViews", token=tok)["Items"]:
            leaks.add(v["Id"])
    return {l.replace("-", "").lower() for l in leaks}


def no_leaks(name, payload, leaks):
    text = (payload if isinstance(payload, (bytes, str)) else json.dumps(payload))
    text = text.decode(errors="replace") if isinstance(text, bytes) else text
    found = [l for l in leaks if l in text.lower().replace("-", "")]
    check(f"{name}: no upstream IDs/addresses", not found, f"leaked {found[:3]}")


def main():
    rig = load_rig()
    leaks = upstream_secrets(rig)
    jb = rig["jellybloom"]

    st, _, info = req("/System/Info/Public")
    check("public info", st == 200 and info["ProductName"] == "Jellyfin Server" and info["Version"].startswith("12."), info)

    st, _, auth = req("/Users/AuthenticateByName", method="POST", body={"Username": jb["username"], "Pw": jb["password"]})
    check("login", st == 200 and auth.get("AccessToken"), st)
    tok, uid = auth["AccessToken"], auth["User"]["Id"]

    st, _, views = req(f"/UserViews?userId={uid}", tok)
    names = sorted(v["Name"] for v in views["Items"])
    check("libraries: Movies merged, plus Shows and Music", names == ["Movies", "Music", "Shows"], names)
    no_leaks("libraries", views, leaks)
    lib = {v["Name"]: v for v in views["Items"]}

    st, _, movies = req(f"/Users/{uid}/Items?ParentId={lib['Movies']['Id']}&Recursive=true&IncludeItemTypes=Movie"
                        "&SortBy=SortName&Fields=PrimaryImageAspectRatio,ProviderIds", tok)
    mnames = [m["Name"] for m in movies["Items"]]
    check("movies deduplicated across servers", sorted(mnames) == ["Colour Bars (1971)", "Night of the Test Pattern"], mnames)
    no_leaks("movie list", movies, leaks)
    shared = next(m for m in movies["Items"] if m["Name"] == "Night of the Test Pattern")

    st, _, top = req(f"/Users/{uid}/Items?ParentId={lib['Movies']['Id']}", tok)
    check("non-recursive library listing", st == 200, st)
    print("     top level of Movies:", [(i["Type"], i["Name"]) for i in top["Items"]])

    st, _, detail = req(f"/Users/{uid}/Items/{shared['Id']}", tok)
    check("item detail", st == 200 and detail["Id"] == shared["Id"] and detail.get("ProductionYear") == 1968, st)
    no_leaks("item detail", detail, leaks)

    st, _, everything = req(f"/Users/{uid}/Items?Recursive=true", tok)
    with_art = [i for i in everything["Items"] if (i.get("ImageTags") or {}).get("Primary")]
    check("items have artwork", bool(with_art), "no item has a primary image")
    for it in with_art[:3]:
        tag = it["ImageTags"]["Primary"]
        st, hdrs, img = req(f"/Items/{it['Id']}/Images/Primary?tag={tag}&maxWidth=200", raw=True)
        check(f"artwork for {it['Name']} (anonymous, routed to its upstream)",
              st == 200 and hdrs.get("Content-Type", "").startswith("image/"), f"{st} {hdrs.get('Content-Type') if hdrs else ''}")

    st, _, series = req(f"/Users/{uid}/Items?ParentId={lib['Shows']['Id']}&Recursive=true&IncludeItemTypes=Series", tok)
    ser = series["Items"][0] if series and series.get("Items") else None
    check("shows", ser is not None and ser["Name"] == "Test Show", series)
    if ser:
        st, _, eps = req(f"/Shows/{ser['Id']}/Episodes?userId={uid}", tok)
        check("episodes in order", [e["IndexNumber"] for e in eps["Items"]] == [1, 2], eps)

    st, _, albums = req(f"/Users/{uid}/Items?ParentId={lib['Music']['Id']}&Recursive=true&IncludeItemTypes=MusicAlbum", tok)
    check("music albums", [a["Name"] for a in albums["Items"]] == ["Pure Tones"], albums)
    st, _, artists = req(f"/Artists/AlbumArtists?parentId={lib['Music']['Id']}&userId={uid}", tok)
    check("album artists", [a["Name"] for a in artists["Items"]] == ["Sine Wave Ensemble"], artists)
    st, _, tracks = req(f"/Users/{uid}/Items?ParentId={albums['Items'][0]['Id']}&SortBy=IndexNumber", tok)
    check("album tracks", [t["Name"] for t in tracks["Items"]] == ["A440", "C523"], tracks)

    st, _, hints = req(f"/Search/Hints?searchTerm=pattern&userId={uid}", tok)
    check("search", any(h["Name"] == "Night of the Test Pattern" for h in hints["SearchHints"]), hints)

    # Playback: the shared movie has a 720p file on both servers and a 480p file on a.
    st, _, pi = req(f"/Items/{shared['Id']}/PlaybackInfo?userId={uid}", tok, method="POST",
                    body={"UserId": uid, "MaxStreamingBitrate": 120000000, "DeviceProfile": {
                        "MaxStreamingBitrate": 120000000,
                        "DirectPlayProfiles": [{"Container": "mkv,mp4", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac"}],
                        "TranscodingProfiles": [{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac",
                                                 "Protocol": "hls", "Context": "Streaming"}]}})
    srcs = pi.get("MediaSources", []) if isinstance(pi, dict) else []
    check("PlaybackInfo: two versions (720p on both, 480p on a)", st == 200 and sorted(s.get("Name") for s in srcs) == ["480p H264", "720p H264"],
          [(s.get("Name"), s.get("Id")) for s in srcs] or pi)
    no_leaks("PlaybackInfo", pi, leaks)
    if not srcs:
        return
    v720 = next(s for s in srcs if s["Name"] == "720p H264")
    psid = pi["PlaySessionId"]

    st, hdrs, body = req(f"/Videos/{shared['Id']}/stream?static=true&MediaSourceId={v720['Id']}&DeviceId=smoke-device-1&api_key={tok}",
                         headers={"Range": "bytes=0-1023"}, raw=True)
    check("direct play range request", st == 206 and len(body) == 1024, f"{st} {len(body)}")

    # Force a transcode to exercise HLS through JellyBloom.
    st, _, pt = req(f"/Items/{shared['Id']}/PlaybackInfo?userId={uid}", tok, method="POST",
                    body={"UserId": uid, "MediaSourceId": v720["Id"], "EnableDirectPlay": False, "EnableDirectStream": False,
                          "DeviceProfile": {"MaxStreamingBitrate": 120000000, "DirectPlayProfiles": [],
                                            "TranscodingProfiles": [{"Container": "ts", "Type": "Video", "VideoCodec": "h264",
                                                                     "AudioCodec": "aac", "Protocol": "hls", "Context": "Streaming"}]}})
    tsrc = (pt.get("MediaSources") or [{}])[0] if isinstance(pt, dict) else {}
    check("narrowed PlaybackInfo keeps the version ID", tsrc.get("Id") == v720["Id"], tsrc.get("Id"))
    no_leaks("transcoding PlaybackInfo", pt, leaks)
    v720, psid = tsrc, pt.get("PlaySessionId", psid)
    if v720.get("TranscodingUrl"):
        st, _, m3u8 = req(v720["TranscodingUrl"], raw=True)
        text = m3u8.decode(errors="replace") if isinstance(m3u8, bytes) else str(m3u8)
        check("HLS master playlist", st == 200 and "#EXTM3U" in text, f"{st} {text[:200]}")
        no_leaks("HLS master playlist", text, leaks)
        variant = next((l for l in text.splitlines() if l and not l.startswith("#")), None)
        if variant:
            base = v720["TranscodingUrl"].split("?")[0].rsplit("/", 1)[0]
            st, _, media = req(base + "/" + variant, raw=True)
            mtext = media.decode(errors="replace") if isinstance(media, bytes) else str(media)
            check("HLS media playlist", st == 200 and "#EXTINF" in mtext, f"{st} {mtext[:200]}")
            no_leaks("HLS media playlist", mtext, leaks)
            seg = next((l for l in mtext.splitlines() if l and not l.startswith("#")), None)
            if seg:
                st, hdrs, sb = req(base + "/" + seg, raw=True)
                check("HLS segment (transcoded on an upstream)", st == 200 and len(sb) > 1000, f"{st} {len(sb)}")
        st, _, _ = req(f"/Videos/ActiveEncodings?DeviceId=smoke-device-1&PlaySessionId={psid}", tok, method="DELETE")
        check("transcode cleanup", st in (200, 204), st)
    else:
        check("TranscodingUrl present", False, v720)

    st, _, _ = req("/Sessions/Playing/Stopped", tok, method="POST",
                   body={"ItemId": shared["Id"], "MediaSourceId": v720["Id"], "PlaySessionId": psid, "PositionTicks": 50000000})
    check("progress report", st in (200, 204), st)

    st, _, _ = req("/System/Logs", tok)
    check("upstream admin routes blocked", st == 403, st)

    print(f"\n{len(failures)} failure(s)" if failures else "\nall checks passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
