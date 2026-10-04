// Package tag writes metadata, cover art and lyrics into decrypted audio.
//
// Both supported containers are written in a single streaming pass: the tag
// section is emitted first and the audio frames are copied after it, so a
// 150 MB track is never rewritten to insert tags.
package tag

import (
	"encoding/binary"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Tags is the metadata written into an output file. Empty fields are skipped.
type Tags struct {
	Title       string
	Artists     []string
	Album       string
	AlbumArtist string

	// Lyrics is the timeline written to the primary lyrics field, with the
	// translation already interleaved so any player renders it bilingually.
	Lyrics string
	// The individual timelines are also stored separately, for players that
	// understand per-language lyric fields.
	LyricsOriginal     string
	LyricsTranslation  string
	LyricsRomanization string

	Cover     []byte
	CoverMIME string

	// The NetEase identifiers the track came from. They are written into the
	// file so the track stays identifiable after it is renamed or moved: the
	// song id is what lets the cover editor offer the official artwork and the
	// lyrics page find the right recording, and the rest is what the info page
	// reads back. Zero values are skipped, so a file decrypted without them
	// simply does not carry the keys.
	MusicID int64
	AlbumID int64
	// ArtistIDs runs parallel to Artists: the n-th id belongs to the n-th name.
	// An entry the metadata did not name is zero and is stored as an empty
	// field, so the positions still line up when the list is read back.
	ArtistIDs []int64
	// Bitrate is the source's bit rate in bits per second, as the .ncm
	// metadata states it.
	Bitrate int
}

// platformKeys are the comment keys this package writes for NetEase's own
// data. They live in one list because several callers have to agree on them:
// the two writers, the reader, and the rewrite that must not drop them.
var platformKeys = []string{
	"NETEASE_MUSIC_ID",
	"NETEASE_ALBUM_ID",
	"NETEASE_ARTIST_IDS",
	"NETEASE_BITRATE",
}

// formatIDs renders an id list the way it is stored: comma-separated, in
// credit order, with unnamed entries left as empty fields.
func formatIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		if id != 0 {
			parts[i] = strconv.FormatInt(id, 10)
		}
	}
	return strings.Join(parts, ",")
}

// parseIDs reads a stored id list. Empty fields become zeros rather than being
// dropped, for the same reason they are written that way.
func parseIDs(s string) []int64 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, _ := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		out = append(out, id)
	}
	return out
}

// ArtistString joins the credited artists for the single-string tag fields.
//
// The separator is a comma rather than the "/" that ID3v2.3 nominally uses for
// multi-value frames: a comma cannot be mistaken for part of a name, whereas
// splitting on "/" would tear "AC/DC" in half.
func (t *Tags) ArtistString() string {
	return strings.Join(t.Artists, ", ")
}

// utf16LE encodes a string as UTF-16 little-endian with a byte order mark,
// the encoding ID3v2.3 requires for text outside Latin-1.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2+2)
	out = append(out, 0xFF, 0xFE) // BOM
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// putUint32BE appends a big-endian uint32.
func putUint32BE(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

// putUint32LE appends a little-endian uint32.
func putUint32LE(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}
