# The traveling library

Take part of your library on a trip, and get a packing list, a check for botched copies,
and an unpacking list from the same setup.

A laptop runs its own small Jellyfin server with a portable copy of what you want for the
trip (an SSD, say), and JellyBloom combines it with your home server. Your TV, phone, or
tablet connects to the laptop's JellyBloom wherever you are.

## What runs on the laptop

`scripts/travel.sh` sets up three JellyBloom instances:

| Instance | Port | Shows |
|----------|------|-------|
| **library** | 8400 | Everything: the laptop's copies and home's, as one library (union) |
| **packing** | 8401 | What home has and the laptop doesn't (home \ laptop) |
| **unpacking** | 8402 | What the laptop has and home doesn't (laptop \ home) |

## Setting up

You need: Jellyfin on the laptop with a library pointing at your travel copies, your home
Jellyfin, and (to carry watch history both ways) a JellyBloom at home too.

```sh
HOME_URL=https://jellyfin.example.com \
HOME_JELLYBLOOM=https://jellybloom.example.com \
USERS_FROM=/path/to/home-jellybloom/config.yaml \
scripts/travel.sh init
scripts/travel.sh start
```

`init` writes the three configs under `~/.jellybloom-travel`, and pairs both servers once
(approve the Quick Connect codes in a Jellyfin app signed in to each server, as the account
JellyBloom should use). If your home JellyBloom calls the home server something other than
`home`, set `HOME_NAME` to that name. Other settings are listed at the top of the script.

## Before you leave

1. **Pack from the list.** Open the packing list (`http://localhost:8401`) in any Jellyfin
   app, or the web app, and copy what you want onto the laptop's library folder. Titles
   leave the list as the laptop's Jellyfin picks them up.
2. **Check the copies.** While you are on the home network, JellyBloom fingerprints every
   title that is on both servers and compares the copies. `scripts/travel.sh report` lists
   any copy that was cut off (an SSD unplugged too early) or is damaged, with its path.
3. **Sync watch state.** `scripts/travel.sh sync`, so Continue Watching is up to date.

## On the road

- **Local copies play first.** The laptop's server is listed first, so it wins until speeds
  are measured, and it is the faster one from then on. Anything not packed streams from home,
  in the same library, with the same Continue Watching row.
- **A bad local file is rescued.** If a copy on the laptop turns out to be damaged mid-play
  and home is reachable, playback continues from home at the same byte.
- **No internet.** Home is marked unreachable and skipped. Everything packed plays; the
  whole catalog is still browsable, and the web app still loads (the laptop serves it).
  Titles only at home fail at once instead of hanging.
- Other devices on the same network (a hotel TV, say) can use the library at
  `http://<laptop's address>:8400`.

## Back home

- **Unpack.** The unpacking list (`http://localhost:8402`) shows what you added on the trip
  (a download, something ripped): copy it home.
- **Sync.** `scripts/travel.sh sync` merges what you watched on the trip into your home
  JellyBloom, and what the family watched at home into the laptop's.

## The caveats, and how they are handled

| Caveat | How it is handled |
|--------|-------------------|
| Watch history lives in the laptop's JellyBloom, and the family at home uses something else meanwhile | Run JellyBloom at home too, with the same users (`USERS_FROM` copies them, so the same people have the same IDs on both). `scripts/travel.sh sync` merges watch state, resume positions, favorites, ratings, and playlists both ways; the most recent change wins per title and playlist, and deletions and "unwatched" carry over too. Run it before leaving, on return, and whenever both are reachable. |
| Until speeds are measured, config order breaks ties | `init` lists the laptop's server first. |
| The packing and unpacking lists keep their own watch state | They are planning tools; watch from the library. |
| The web app would come from home, which is unreachable offline | `init` makes the laptop's server serve it. |
| Titles only at home cannot play offline | That is what the packing list is for; the report also lists every title with only one copy. |
| Not tested end to end | `testrig/travel.py` runs the whole scenario on the test rig: setup through the script, packing and unpacking lists, the copy check, local-first play, rescuing a damaged copy, going offline, and syncing (including undoing). |

## Syncing details

`jellybloom sync -user NAME URL_A URL_B` (what `travel.sh sync` runs) logs in to both
instances as an admin user and calls `GET` and `POST /JellyBloom/UserData` on each (admin
users only). Items are matched by their match keys (TMDb, TVDb, MusicBrainz IDs, or title
and year), so an instance that knows a title under another ID still finds it; state for a
title the other side doesn't have yet is kept, and appears when the title does.
For a title watched in both places while apart, the more recent change wins as a whole
(position, played, play count); the counts are not added.

Set `SYNC_PASSWORD_FILE` to a file only you can read to run `sync` without a prompt (for
example from cron or launchd while the laptop is home).
