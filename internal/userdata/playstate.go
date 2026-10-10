package userdata

import "time"

// Jellyfin's default resume thresholds.
const (
	MinResumePct             = 5   // below this, a stop is not resumable
	MaxResumePct             = 90  // above this, the item counts as played
	MinResumeDurationSeconds = 300 // shorter items are not resumable: stopping counts as played
)

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

// Apply updates r for a playback report about an item with runtime ticks
// (0 when unknown), following Jellyfin: a start counts a play; progress keeps
// the position; a stop past MaxResumePct, of a short item, or of an item
// without a runtime marks it played and clears the position, while a stop
// before MinResumePct clears the position.
func Apply(r *Record, rep Report, runtime int64, now time.Time) {
	if rep.AudioStream != nil {
		r.AudioStream = rep.AudioStream
	}
	if rep.SubtitleStream != nil {
		r.SubtitleStream = rep.SubtitleStream
	}
	switch rep.Event {
	case Start:
		r.PlayCount++
		r.LastPlayed = now
		if rep.PositionTicks != nil {
			r.Position = *rep.PositionTicks
		}
	case Progress:
		r.LastPlayed = now
		if rep.PositionTicks != nil {
			r.Position = *rep.PositionTicks
		}
	case Stop:
		r.LastPlayed = now
		if rep.PositionTicks == nil {
			return
		}
		pos := *rep.PositionTicks
		played := false
		switch {
		case runtime <= 0:
			pos, played = 0, true
		case pos <= 0:
			// Stopped at the very start: nothing to resume.
		default:
			pct := float64(pos) / float64(runtime) * 100
			switch {
			case pct < MinResumePct:
				pos = 0
			case pct > MaxResumePct:
				pos, played = 0, true
			case runtime/10_000_000 < MinResumeDurationSeconds:
				pos, played = 0, true
			}
		}
		r.Position = pos
		if played {
			r.Played = true
		}
	}
}

// MarkPlayed records an item as played (at when, if given).
func MarkPlayed(r *Record, when time.Time) {
	r.Played = true
	r.Position = 0
	r.PlayCount = max(r.PlayCount, 1)
	r.LastPlayed = when
}

// MarkUnplayed clears played state and the resume position.
func MarkUnplayed(r *Record) {
	r.Played = false
	r.Position = 0
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
