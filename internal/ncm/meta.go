package ncm

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Artist is one entry of the metadata's artist list.
type Artist struct {
	Name string
	ID   int64
}

// Meta is the JSON payload hidden in the container's metadata blob.
//
// It carries no lyrics of any kind — the format has no lyric, lrc or yrc
// field. Lyrics only exist server-side, which is why they are fetched
// separately using MusicID.
type Meta struct {
	Format     string   `json:"format"`
	MusicID    int64    `json:"musicId"`
	MusicName  string   `json:"musicName"`
	Album      string   `json:"album"`
	AlbumID    int64    `json:"albumId"`
	AlbumPic   string   `json:"albumPic"`
	MVId       int64    `json:"mvId"`
	Bitrate    int      `json:"bitrate"`
	DurationMS int      `json:"duration"` // milliseconds
	Gain       float64  `json:"gain"`
	Alias      []string `json:"alias"`      // alternate titles
	TransNames []string `json:"transNames"` // translated titles

	Artists []Artist `json:"-"`
}

// rawMeta mirrors Meta but keeps artist undecoded, because the field is either
// a plain string or a list of [name, id] pairs depending on the client version.
type rawMeta struct {
	Format     string          `json:"format"`
	MusicID    int64           `json:"musicId"`
	MusicName  string          `json:"musicName"`
	Artist     json.RawMessage `json:"artist"`
	Album      string          `json:"album"`
	AlbumID    int64           `json:"albumId"`
	AlbumPic   string          `json:"albumPic"`
	MVId       int64           `json:"mvId"`
	Bitrate    int             `json:"bitrate"`
	DurationMS int             `json:"duration"`
	Gain       float64         `json:"gain"`
	Alias      []string        `json:"alias"`
	TransNames []string        `json:"transNames"`
}

func parseMeta(data []byte) (*Meta, error) {
	var raw rawMeta
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	m := &Meta{
		Format:     raw.Format,
		MusicID:    raw.MusicID,
		MusicName:  raw.MusicName,
		Album:      raw.Album,
		AlbumID:    raw.AlbumID,
		AlbumPic:   raw.AlbumPic,
		MVId:       raw.MVId,
		Bitrate:    raw.Bitrate,
		DurationMS: raw.DurationMS,
		Gain:       raw.Gain,
		Alias:      raw.Alias,
		TransNames: raw.TransNames,
	}
	m.Artists = parseArtists(raw.Artist)
	return m, nil
}

// parseArtists handles every shape the artist field takes: a bare string, a
// list of names, or a list of [name, id] pairs. Feature-heavy tracks carry
// several entries, and dropping all but the first is a common way to lose
// credited artists.
func parseArtists(raw json.RawMessage) []Artist {
	if len(raw) == 0 {
		return nil
	}
	var solo string
	if err := json.Unmarshal(raw, &solo); err == nil {
		if solo == "" {
			return nil
		}
		return []Artist{{Name: solo}}
	}

	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		out := make([]Artist, 0, len(names))
		for _, n := range names {
			if n != "" {
				out = append(out, Artist{Name: n})
			}
		}
		return out
	}

	var pairs [][]json.RawMessage
	if err := json.Unmarshal(raw, &pairs); err != nil {
		return nil
	}
	out := make([]Artist, 0, len(pairs))
	for _, pair := range pairs {
		if len(pair) == 0 {
			continue
		}
		var name string
		if err := json.Unmarshal(pair[0], &name); err != nil || name == "" {
			continue
		}
		a := Artist{Name: name}
		if len(pair) > 1 {
			if err := json.Unmarshal(pair[1], &a.ID); err != nil {
				// A non-numeric id is not worth failing the track over.
				var s string
				if json.Unmarshal(pair[1], &s) == nil {
					a.ID, _ = strconv.ParseInt(s, 10, 64)
				}
			}
		}
		out = append(out, a)
	}
	return out
}

// ArtistNames returns every credited artist, in order.
func (m *Meta) ArtistNames() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.Artists))
	for _, a := range m.Artists {
		if a.Name != "" {
			out = append(out, a.Name)
		}
	}
	return out
}

// ArtistString joins the credited artists the way file names usually do.
func (m *Meta) ArtistString() string {
	return strings.Join(m.ArtistNames(), ", ")
}

// Title returns the song title, falling back to a translated name.
func (m *Meta) Title() string {
	if m == nil {
		return ""
	}
	if m.MusicName != "" {
		return m.MusicName
	}
	if len(m.TransNames) > 0 {
		return m.TransNames[0]
	}
	return ""
}

// DisplayName is the "Artist - Title" form used for output file names.
func (m *Meta) DisplayName() string {
	if m == nil {
		return ""
	}
	title := m.Title()
	artist := m.ArtistString()
	switch {
	case artist != "" && title != "":
		return artist + " - " + title
	case title != "":
		return title
	default:
		return artist
	}
}
