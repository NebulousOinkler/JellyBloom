package userdata

import "time"

// Jellyfin's default resume thresholds.
const (
	MinResumePct             = 5   // below this, no resume point is kept
	MaxResumePct             = 90  // above this, the item counts as played
	MinResumeDurationSeconds = 300 // shorter items are not resumable: a resume point counts as played
	MinAudiobookResumeMin    = 5   // audiobooks: no resume point in the first minutes
	MaxAudiobookResumeMin    = 5   // audiobooks: played with fewer minutes left
)

// Kind selects the playback rules for an item.
type Kind int

const (
	KindVideo     Kind = iota // percentage rules, resumable
	KindAudio                 // music: played when it starts, never resumable
	KindAudioBook             // minute rules, resumable
)

// Item is what Apply needs to know about the item played.
type Item struct {
	Runtime int64 // ticks; 0 when unknown
	Kind    Kind
}

// Event is a playback report.
type Event int

const (
	Start Event = iota
	Progress
	Stop
)

// Report is what a client tells the server during playback.
type Report struct {
	Event          Event
	PositionTicks  *int64
	AudioStream    *int
	SubtitleStream *int
}

// Apply updates r for a playback report, as Jellyfin 12.2 does (checked
// against a real server):
//   - start counts a play and leaves the position alone; it dates the play
//     only when the item is not already played, and plays music to completion;
//   - progress and stop with a position apply the resume rules, dating the
//     play when a resume point is kept or the item completes;
//   - stop without a position counts a full play.
func Apply(r *Record, rep Report, item Item, now time.Time) {
	if rep.AudioStream != nil {
		r.AudioStream = rep.AudioStream
	}
	if rep.SubtitleStream != nil {
		r.SubtitleStream = rep.SubtitleStream
	}
	switch rep.Event {
	case Start:
		r.PlayCount++
		if !r.Played {
			r.LastPlayed = now
		}
		if item.Kind == KindAudio {
			r.Played = true
		}
	case Progress, Stop:
		if rep.PositionTicks == nil {
			if rep.Event == Stop {
				r.PlayCount++
				r.Played, r.Position, r.LastPlayed = true, 0, now
			}
			return
		}
		if completed := setPosition(r, *rep.PositionTicks, item); completed || r.Position > 0 {
			r.LastPlayed = now
		}
	}
}

// setPosition applies Jellyfin's resume rules to a reported position and
// reports whether the item was played to completion.
func setPosition(r *Record, pos int64, item Item) (completed bool) {
	rt := item.Runtime
	switch {
	case rt <= 0:
		pos, completed = 0, true // unknown runtime: assume it was played through
	case pos <= 0:
		pos = 0
	case item.Kind == KindAudioBook:
		switch {
		case pos < MinAudiobookResumeMin*60*10_000_000:
			pos = 0
		case rt-pos < MaxAudiobookResumeMin*60*10_000_000:
			pos, completed = 0, true
		}
	default:
		pct := float64(pos) / float64(rt) * 100
		switch {
		case pct < MinResumePct:
			pos = 0
		case pct > MaxResumePct || pos >= rt:
			pos, completed = 0, true
		case rt/10_000_000 < MinResumeDurationSeconds:
			pos, completed = 0, true
		}
	}
	if item.Kind == KindAudio {
		pos = 0
	}
	r.Position = pos
	if completed {
		r.Played = true
	}
	return completed
}

// MarkPlayed records an item as played. With a date, it counts a play on
// that date; without, it keeps the last played date if there is one.
func MarkPlayed(r *Record, datePlayed *time.Time, now time.Time) {
	switch {
	case datePlayed != nil:
		r.PlayCount++
		r.LastPlayed = *datePlayed
	case r.LastPlayed.IsZero():
		r.LastPlayed = now
	}
	r.PlayCount = max(r.PlayCount, 1)
	r.Played = true
	r.Position = 0
}

// MarkUnplayed clears played state, play count, last played, and the resume
// position.
func MarkUnplayed(r *Record) {
	r.Played = false
	r.PlayCount = 0
	r.Position = 0
	r.LastPlayed = time.Time{}
}

// DTO renders r as a Jellyfin UserItemDataDto for itemID.
func (r Record) DTO(itemID string, runtime int64) map[string]any {
	d := map[string]any{
		"PlaybackPositionTicks": r.Position,
		"PlayCount":             r.PlayCount,
		"IsFavorite":            r.IsFavorite,
		"Played":                r.Played,
		"Key":                   itemID,
		"ItemId":                itemID,
	}
	if r.Likes != nil {
		d["Likes"] = *r.Likes
	}
	if !r.LastPlayed.IsZero() {
		d["LastPlayedDate"] = r.LastPlayed.UTC().Format(time.RFC3339Nano)
	}
	if r.Position > 0 && runtime > 0 {
		d["PlayedPercentage"] = float64(r.Position) / float64(runtime) * 100
	}
	return d
}
