#!/usr/bin/env python3
"""Drives JellyBloom like a Jellyfin client against the local test rig.

Checks login, merged libraries, browsing, details, artwork, PlaybackInfo
versions, HLS, direct play, music, audiobooks, upstream collections, and user
state, and that no upstream IDs, tokens, or addresses reach the client.
Prints PASS/FAIL per check; exits 1 on failure. testrig/compare.py checks
user-state behavior against real Jellyfin.

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


def user_state(tok, uid, movie, series, leaks):
    """Phase 7: watched state, resume, next up, favorites, playlists."""
    mid = movie["Id"]
    for path in (f"/UserPlayedItems/{mid}", f"/UserFavoriteItems/{mid}"):  # start clean
        req(path, tok, method="DELETE")
    req(f"/Sessions/Playing", tok, method="POST", body={"ItemId": mid, "PositionTicks": 0})
    runtime = movie.get("RunTimeTicks") or req(f"/Users/{uid}/Items/{mid}", tok)[2].get("RunTimeTicks", 0)
    req(f"/Sessions/Playing/Progress", tok, method="POST", body={"ItemId": mid, "PositionTicks": runtime // 2})
    st, _, res = req(f"/Users/{uid}/Items/Resume?MediaTypes=Video", tok)
    check("resume after stopping half way", any(i["Id"] == mid for i in res.get("Items", [])), res)
    req(f"/Sessions/Playing/Stopped", tok, method="POST", body={"ItemId": mid, "PositionTicks": runtime * 95 // 100})
    _, _, d = req(f"/Users/{uid}/Items/{mid}", tok)
    check("watched after stopping at 95%", d["UserData"]["Played"] is True and d["UserData"]["PlaybackPositionTicks"] == 0, d["UserData"])
    no_leaks("item with user data", d, leaks)

    if series:
        _, _, eps = req(f"/Shows/{series['Id']}/Episodes?userId={uid}", tok)
        e1 = eps["Items"][0]["Id"]
        for e in eps["Items"]:
            req(f"/UserPlayedItems/{e['Id']}", tok, method="DELETE")
        req(f"/UserPlayedItems/{e1}", tok, method="POST")
        _, _, nu = req(f"/Shows/NextUp?userId={uid}", tok)
        check("next up is episode 2 after watching episode 1", [i.get("IndexNumber") for i in nu["Items"]] == [2], nu)
        _, _, sd = req(f"/Users/{uid}/Items/{series['Id']}", tok)
        n = len(eps["Items"]) - 1
        check(f"series shows {n} unplayed episodes", sd["UserData"].get("UnplayedItemCount") == n, sd["UserData"])
        req(f"/Users/{uid}/PlayedItems/{series['Id']}", tok, method="POST")  # legacy route, whole series
        _, _, sd = req(f"/Users/{uid}/Items/{series['Id']}", tok)
        check("marking the series played marks every episode", sd["UserData"].get("Played") is True, sd["UserData"])

    for it in req(f"/Users/{uid}/Items?Recursive=true&Filters=IsFavorite", tok)[2]["Items"]:  # start clean
        req(f"/UserFavoriteItems/{it['Id']}", tok, method="DELETE")
    req(f"/UserFavoriteItems/{mid}", tok, method="POST")
    _, _, favs = req(f"/Users/{uid}/Items?Recursive=true&Filters=IsFavorite", tok)
    check("favorites filter", [i["Id"] for i in favs["Items"]] == [mid], favs)

    if series:
        ep_ids = [e["Id"] for e in eps["Items"]]
        st, _, mix = req("/Playlists", tok, method="POST", body={"Name": "Shuffle Mix", "Ids": ep_ids, "UserId": uid})
        orders = {tuple(i["Id"] for i in req(f"/Users/{uid}/Items?ParentId={mix['Id']}&SortBy=Random", tok)[2]["Items"]) for _ in range(8)}
        check("shuffling a playlist (SortBy=Random)", len(orders) > 1 and all(sorted(o) == sorted(ep_ids) for o in orders), orders)
        req(f"/Items/{mix['Id']}", tok, method="DELETE")

    st, _, pl = req("/Playlists", tok, method="POST", body={"Name": "Smoke Mix", "Ids": [mid], "UserId": uid})
    pid = pl.get("Id") if isinstance(pl, dict) else None
    check("create playlist", st == 200 and pid, pl)
    if pid:
        _, _, items = req(f"/Playlists/{pid}/Items?userId={uid}", tok)
        check("playlist lists its item", [i["Id"] for i in items["Items"]] == [mid] and items["Items"][0].get("PlaylistItemId"), items)
        _, _, views = req(f"/UserViews?userId={uid}", tok)
        check("Playlists library appears", any(v.get("CollectionType") == "playlists" for v in views["Items"]), views)
        st, _, _ = req(f"/Items/{pid}", tok, method="DELETE")
        check("delete playlist", st == 204, st)
    st, _, _ = req(f"/Items/{mid}", tok, method="DELETE")
    check("deleting upstream media is refused", st == 403, st)


def upstream_collection(tok, uid, library, rig, leaks):
    """A collection made on server a: members are linked, and edits stay in JellyBloom."""
    _, _, cols = req(f"/Users/{uid}/Items?ParentId={library['Id']}", tok)
    col = next((c for c in cols.get("Items", []) if c["Name"] == "Test Pattern Collection"), None)
    check("upstream collection listed", col is not None and col["Type"] == "BoxSet", cols)
    if not col:
        return
    members = lambda: sorted(i["Name"] for i in req(f"/Users/{uid}/Items?ParentId={col['Id']}", tok)[2]["Items"])
    check("upstream collection members", members() == ["Colour Bars (1971)", "Long Test Pattern"], members())
    _, _, night = req(f"/Users/{uid}/Items?Recursive=true&IncludeItemTypes=Movie&SearchTerm=Night", tok)
    nid = night["Items"][0]["Id"]
    st, _, _ = req(f"/Collections/{col['Id']}/Items?ids={nid}", tok, method="POST")
    got = members()
    req(f"/Collections/{col['Id']}/Items?ids={nid}", tok, method="DELETE")
    check("adding to an upstream collection", st == 204 and "Night of the Test Pattern" in got, f"{st} {got}")
    a = rig["upstreams"]["a"]
    atok = call(a["url"], "POST", "/Users/AuthenticateByName", {"Username": a["username"], "Pw": a["password"]})["AccessToken"]
    upcol = call(a["url"], "GET", "/Items", token=atok, query={"Recursive": "true", "IncludeItemTypes": "BoxSet"})["Items"][0]
    upm = sorted(i["Name"] for i in call(a["url"], "GET", "/Items", token=atok, query={"ParentId": upcol["Id"]})["Items"])
    check("the upstream collection itself is unchanged", upm == ["Colour Bars (1971)", "Long Test Pattern"], upm)
    no_leaks("upstream collection", cols, leaks)


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
    check("libraries: Movies merged, plus Shows, Music, Books, and a's Collections",
          names == ["Books", "Collections", "Movies", "Music", "Shows"], names)
    no_leaks("libraries", views, leaks)
    lib = {v["Name"]: v for v in views["Items"]}

    st, _, movies = req(f"/Users/{uid}/Items?ParentId={lib['Movies']['Id']}&Recursive=true&IncludeItemTypes=Movie"
                        "&SortBy=SortName&Fields=PrimaryImageAspectRatio,ProviderIds", tok)
    mnames = [m["Name"] for m in movies["Items"]]
    check("movies deduplicated across servers", sorted(mnames) == ["Colour Bars (1971)", "Damaged Pattern", "Long Test Pattern",
                                                                   "Night of the Test Pattern", "Truncated Pattern"], mnames)
    no_leaks("movie list", movies, leaks)
    shared = next(m for m in movies["Items"] if m["Name"] == "Night of the Test Pattern")
    long_movie = next((m for m in movies["Items"] if m["Name"] == "Long Test Pattern"), None)

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
        check("episodes in order", [(e["ParentIndexNumber"], e["IndexNumber"]) for e in eps["Items"]] == [(1, 1), (1, 2), (1, 3), (1, 4), (2, 1)],
              [(e.get("ParentIndexNumber"), e.get("IndexNumber")) for e in eps["Items"]])

    st, _, albums = req(f"/Users/{uid}/Items?ParentId={lib['Music']['Id']}&Recursive=true&IncludeItemTypes=MusicAlbum", tok)
    check("music albums", sorted(a["Name"] for a in albums["Items"]) == ["Long Tones", "Pure Tones"], albums)
    st, _, artists = req(f"/Artists/AlbumArtists?parentId={lib['Music']['Id']}&userId={uid}", tok)
    check("album artists", [a["Name"] for a in artists["Items"]] == ["Sine Wave Ensemble"], artists)
    pure = next(a for a in albums["Items"] if a["Name"] == "Pure Tones")
    st, _, tracks = req(f"/Users/{uid}/Items?ParentId={pure['Id']}&SortBy=IndexNumber", tok)
    check("album tracks", [t["Name"] for t in tracks["Items"]] == ["A440", "C523"], tracks)

    st, _, books = req(f"/Users/{uid}/Items?ParentId={lib['Books']['Id']}&Recursive=true&IncludeItemTypes=AudioBook", tok)
    check("audiobooks", [b["Name"] for b in books["Items"]] == ["The Long Tone"], books)

    upstream_collection(tok, uid, lib["Collections"], rig, leaks)

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

    if long_movie:
        st, _, lp = req(f"/Items/{long_movie['Id']}/PlaybackInfo?userId={uid}", tok, method="POST", body={"UserId": uid})
        check("identical copies on both servers are one version", st == 200 and len(lp.get("MediaSources", [])) == 1,
              [(s.get("Name"), s.get("Id")) for s in lp.get("MediaSources", [])] if isinstance(lp, dict) else lp)

    # Resume needs an item of 5 minutes or more (Jellyfin's MinResumeDurationSeconds).
    user_state(tok, uid, long_movie or shared, ser, leaks)

    print(f"\n{len(failures)} failure(s)" if failures else "\nall checks passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
