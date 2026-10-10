#!/usr/bin/env python3
"""Lists what each test server indexed (type, name, ids) without printing secrets."""
import json, os, sys, urllib.request
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from setup import call, load_rig, SERVERS

rig = load_rig()
for name, spec in SERVERS.items():
    c = rig["upstreams"][name]
    tok = call(spec["url"], "POST", "/Users/AuthenticateByName", {"Username": c["username"], "Pw": c["password"]})["AccessToken"]
    views = call(spec["url"], "GET", "/UserViews", token=tok)["Items"]
    print(f"== {name}: views " + ", ".join(f'{v["Name"]}({v.get("CollectionType")})' for v in views))
    items = call(spec["url"], "GET", "/Items", token=tok, query={"Recursive": "true", "Fields": "ProviderIds,ParentId,MediaSources"})["Items"]
    for it in items:
        ms = len(it.get("MediaSources") or [])
        print(f'   {it["Type"]:12} {it["Name"][:40]:40} year={it.get("ProductionYear")} prov={it.get("ProviderIds")} sources={ms}')
