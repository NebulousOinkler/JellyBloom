# The Commons (plan, not started)

A group's media, kept alive by the group. Each member runs their own Jellyfin server with
whatever they have; each household runs a JellyBloom that combines every member's server.
Everyone sees the whole collection as one ordinary Jellyfin server, every title plays from
the nearest healthy copy, and JellyBloom keeps the collection healthy: it counts verified
copies of every title, finds damaged ones by comparing copies, and tells members what to
copy, keep, repair, or delete. It never moves a file itself.

It is LOCKSS ("Lots Of Copies Keep Stuff Safe", the system research libraries use to
preserve journals) for films, shows, music, audiobooks, books, comics, and documents.

Status: planned 2026-10-10. Start with the decisions at the end.

## What exists, and what is missing

Already built (phases 1 to 9):
- Combining any number of servers (union), and per-member "what I lack" lists (difference)
- Fingerprints of copies (17 sampled blocks), copy verdicts (truncated, damaged, disputed),
  majority vote between three or more copies, block hashes taken during playback
- The redundancy report: items with a good copy on only one upstream, broken copies
- Failover mid-stream, ranking by health and speed, quarantine, reachability
- Relaying downloads (`/Items/{id}/Download`) from the best copy, with failover
- Syncing user state between JellyBloom instances

Missing for the Commons:
- **Books, audiobooks, photos, and home videos never match across servers**: they have no
  matching rule (`internal/identity/keys.go`), so the same book on two servers is two items
  and is never counted as two copies. The verifier also ignores books (`fileTypes`).
- **Files without good metadata** (home recordings, scans, untagged music, obscure films)
  cannot be matched at all; they need matching by content.
- **Metadata edits look like damage.** Two copies of a FLAC album with different tags differ
  only where the tags are; with three copies, the odd one would be voted "damaged". Same for
  EPUB metadata and MKV tags.
- **Appended edits look like truncation.** A PDF saved with an incremental update is the
  original plus appended bytes, so the original looks like a cut-off prefix of it.
- **No notion of members, locations, a replication target, or a plan** (who should copy or
  may delete what).

## Media kinds

Jellyfin is the transport for everything: whatever a member's Jellyfin indexes can join the
Commons, and whatever it can't index, can't. All of these play or open in stock Jellyfin
apps (jellyfin-web has readers for EPUB, PDF, and comics).

| Kind | Jellyfin library | Item type | File read through | Matched by |
|------|------------------|-----------|-------------------|------------|
| Films, shows, music videos | Movies, Shows, Music videos | Movie, Episode, MusicVideo | static stream | TMDb/TVDb/IMDb IDs; title and year; content |
| Music | Music | Audio (albums, artists above) | static stream | MusicBrainz recording/track; album + track + title; content |
| Audiobooks | Books | AudioBook | static stream | provider IDs if a plugin sets them; author + title; content |
| Books, comics | Books | Book | Download | provider IDs (ISBN, Google Books, ComicVine, if present); author + title + year; content |
| Documents | Books (PDF, EPUB) | Book | Download | content (title and author when the PDF has them) |
| Scans, photos | Photos | Photo | Download | content |
| Home recordings | Home videos | Video | static stream | content |

Documents in formats Jellyfin doesn't index (Word, ODT, plain text) need converting first:
PDF/A, the archival PDF standard, is the recommendation (see decisions).

To verify on the rig before building (C0): what Jellyfin reports for Book items (provider
IDs, size, whether there are MediaSources), Range support and the download permission on
`/Items/{id}/Download` for books and photos, and how comics and PDFs appear.

## Design

### Members and policy: the manifest
A group shares one manifest (YAML), and each member's JellyBloom loads it:
- **Policy:** the target number of good copies of every title (e.g. 3), the minimum number
  of distinct locations (e.g. 2), which libraries count (a member may share only some),
  freshness (how recently a copy must have been verified to count), and the departure grace
  period.
- **Members:** name, the upstream(s) they run, location (a free-form tag like a city), how
  much space they donate, a contact, and a status (active, or leaving).
- Each JellyBloom's own config says which member it belongs to (`self`).

Every member's JellyBloom computes the same plan from the same manifest and the same view
of the copies, so no central server and no coordination protocol is needed.

### Matching every kind
- **Books and audiobooks get matching rules:** strong keys from whatever provider IDs
  Jellyfin or its plugins set (ISBN, Google Books, ComicVine, Audible), and a weak key of
  normalized author + title (+ year for books).
- **Matching by content** for everything else: group unmatched items of the same type across
  different upstreams by exact file size (cheap: sizes come from the index or a one-byte
  probe), fingerprint only the files whose sizes collide, and link files with equal
  fingerprints through a content key (`content:<size>:<digest>`), which identity treats as a
  strong key. The verifier records links; identity reads them at its next rebuild. Content
  matching never crosses item types.

### Comparing copies, by kind
- **Byte-identical:** as today. Only these may be spliced mid-stream.
- **Same content, different metadata:** copies whose differences are confined to the
  file's metadata region (the first and last blocks: ID3/FLAC/MP4 tags, MKV headers, an
  EPUB's container entries) are the same work with different tags. They count as copies for
  preservation, are never spliced together, and are never voted "damaged" for it.
  Format-aware checks (skipping the tag area of FLAC, MP3, M4A) can sharpen this later.
- **Truncation, format-aware:** a prefix of a longer copy is truncated only if its own end is
  not a valid end for its format: a PDF without a final `%%EOF` (an earlier PDF revision ends
  with one), a ZIP-based file (EPUB, CBZ) without an end-of-central-directory record, media
  that claims a longer runtime than it holds (today's rule).
- **Lossless and lossy:** a FLAC and an MP3 of the same recording are different versions.
  Policy decides whether lossy copies count toward the target when a lossless one exists.

### Counting copies
For every work (and every version of it), count the good copies: verified within the
freshness window, no broken verdict, not quarantined, held by an active member. Count
distinct members toward the target and distinct locations toward diversity (two servers in
one house are one location). Roll up containers: "Album: 9 of 12 tracks below target",
seasons and series, book and comic series.

### The plan
- **Endangered:** works below the target or below the location minimum.
- **Assignments:** each endangered work goes to the members who should adopt it, chosen by
  rendezvous hashing (every member computes the same ranking without talking to the others),
  weighted by donated space still free and preferring new locations. Assignments are
  spread, so ten members don't all copy the same film.
- **Surplus:** works above the target, and which holders may delete their copy: only while
  at least the target number of fresh, verified copies in enough locations would remain,
  and preferring members over their donation. Deletion is always a suggestion; JellyBloom
  deletes nothing.
- **Repairs:** a damaged or truncated copy, and the member whose good copy to take.
- **Departures:** a member unreachable longer than the grace period, or marked leaving in the
  manifest, stops counting, so their titles become endangered and get re-copied while they
  still play from the remaining copies (and from the member's, if they come back).

### In the apps
Admin users (members) get extra libraries in any Jellyfin app: **To adopt** (assigned to
me), **Endangered**, **To repair** (my broken copies), and **Safe to free** (my surplus).
Ordinary users of a household never see them. Adopting or repairing is a download: the
Download button relays the file from the best good copy, with failover; the member puts it
in their library, their Jellyfin indexes it, the verifier checks it, and it counts.

### Trust and privacy
- Each member's JellyBloom reads and fingerprints the bytes itself; nothing a member claims
  about their files is trusted. A copy counts only once verified.
- A careless or colluding majority could outvote a good copy: the Commons is for groups that
  trust each other, and disputes with no evidence stay disputes (both kept).
- Members see which member holds what, but not other members' file paths (neutral paths in
  the apps; the report shows real paths only for your own copies).
- Each member pairs with every other member's server through a dedicated non-admin account
  that sees only the libraries they share, with downloads allowed.

## Build phases

| # | Phase | Deliverables | Done when |
|---|-------|--------------|-----------|
| C0 | Rig and facts | A third Jellyfin server (c) on the rig; a corpus across all kinds: an album tagged differently on one server and lossy on another, an EPUB, a PDF and its appended revision, a CBZ, scans in a photos library, a home video without IDs, a damaged and a truncated copy among three; what Jellyfin reports for books and photos, verified and written down | The corpus is indexed on a, b, and c; the facts table above is confirmed or corrected |
| C1 | Members and policy | Manifest format, loading (file or URL), validation, `self`, locations, donations, status, shared libraries; members and their reachability in the report | Every member's JellyBloom shows the same member list and policy |
| C2 | Every kind of file | Books, audiobooks, photos, and home videos in the verifier; copies read through Download when there are no media sources; matching rules for books and audiobooks | The same EPUB, comic, and audiobook on two servers are one item with two copies |
| C3 | Matching by content | Size buckets, fingerprints of collisions, content keys into identity, paced | A home video, a PDF, a scan, and an untagged FLAC on two servers each become one item |
| C4 | Comparing by kind | Metadata-only differences recognized and never voted damaged; format-aware truncation; lossless/lossy policy | A retagged album is three good copies, not one damaged; a PDF's earlier revision is not "truncated"; a cut-off EPUB still is |
| C5 | Counting copies | Good copies per work and version by member and location; freshness; rollups | Counts on the rig match a hand count for every kind |
| C6 | The plan | Endangered, assignments (rendezvous hashing), surplus with safety rules, repairs, departures; `jellybloom commons` CLI and report sections | Taking a member offline past the grace period makes their titles endangered and assigns them to the others, the same on every member |
| C7 | In the apps | Admin-only libraries (To adopt, Endangered, To repair, Safe to free); adopting by download through the relay | A member adopts a title from the To adopt library in jellyfin-web, and it leaves every member's list once verified |
| C8 | Watching it | Metrics (endangered works, members reachable, verified coverage), notifications on decision, a guide for members (joining, accounts, permissions), docs | A title becoming endangered is visible in metrics and (if chosen) notified |

### Rig scenarios (testrig/commons.py)
1. Three members, every kind: counts match a hand count; nothing endangered at target 2.
2. Member c leaves (manifest) and member b goes offline past the grace period: their titles
   become endangered and are assigned, identically on every member's JellyBloom.
3. Bit rot on one of three copies (zeroed block, and random bytes): the odd copy is damaged
   by vote, playback avoids it, a repair names the good source.
4. A retagged FLAC album on one server: three good copies, no damage.
5. A PDF and its appended revision: two versions, neither truncated; an EPUB cut short:
   truncated.
6. A home video and scans with no metadata: matched by content.
7. Adoption: download an assigned title through JellyBloom into member a's library; once
   indexed and verified it counts, and leaves everyone's lists.
8. Surplus: at target 2 with four copies, two holders are told they may free space, never
   leaving fewer than two verified copies in two locations.

## Decisions needed before starting (recommendations first)
1. **What counts as a copy:** the best version of a work, with metadata-only variants
   counting and lossy copies not counting toward a lossless target / any version counts /
   byte-identical copies only
2. **Defaults:** 3 copies in at least 2 locations / 2 copies anywhere
3. **Where the manifest lives:** one shared file at a URL (e.g. a private git repository),
   with `self` in each config / pasted into each member's config
4. **Who adopts what:** deterministic assignment (rendezvous hashing) / members claim titles,
   synced between instances
5. **Departure grace period:** 14 days / 7 / 30
6. **Audit depth:** sampled fingerprints plus playback block hashes (today) / plus a rotating
   full-read audit of a fixed amount per month, covering everything over time
7. **Bulk adoption:** an explicit `jellybloom commons fetch DIR` command that downloads your
   assigned titles into a folder you choose (the server itself still never stores files) /
   downloads from the apps only
8. **Documents Jellyfin can't index:** convert to PDF/A / add a plain-file upstream type
   alongside Jellyfin
9. **Notifications:** a webhook (ntfy, Discord, Slack) / none / email

## Risks and open questions
- **Rights:** the Commons suits material the group may share (public-domain films, home and
  community recordings, Creative Commons work, documents members own).
- **Jellyfin's handling of books and PDFs** is unverified here (C0 settles it).
- **Content matching in large music libraries:** size collisions keep fingerprinting cheap;
  retagged files whose size also changed are not found by content and rely on tags.
- **Members' views differ** while some servers are unreachable, so assignments may differ
  briefly; they converge once everyone sees everyone.
- **Scale:** fingerprinting costs about 1 MiB of reads per file, once (resampled monthly);
  ten members with 10 TB each is a background job of days, paced.
