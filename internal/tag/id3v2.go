package tag

// ID3v2.3 tag writing for MP3 output.
//
import (
	"bytes"
	"io"
	"strconv"
)

// Version 2.3 rather than 2.4: it is what the widest range of players on
// Android and elsewhere parses without complaint, and nothing here needs a
// 2.4-only feature. The trade-off is that non-Latin text must be UTF-16 with a
// byte order mark rather than UTF-8.

const (
	id3v2HeaderSize = 10
	id3v2Version    = 3
	// A little slack so a player or tagger can rewrite the tag in place
	// without having to move the audio frames.
	id3v2Padding = 1024

	frameTitle       = "TIT2"
	frameArtists     = "TPE1"
	frameAlbum       = "TALB"
	frameAlbumArtist = "TPE2"
	frameUserText    = "TXXX"
	framePicture     = "APIC"
	frameLyrics      = "USLT"
)

// BuildID3v2 renders a complete ID3v2.3 tag for the given metadata. It returns
// nil when there is nothing worth writing.
//
// It is the in-memory form of WriteID3v2, for callers that want the bytes.
func BuildID3v2(t *Tags) []byte {
	var buf bytes.Buffer
	// A bytes.Buffer cannot fail, so an error here means the tag is empty.
	if err := WriteID3v2(&buf, t); err != nil || buf.Len() == 0 {
		return nil
	}
	return buf.Bytes()
}

// WriteID3v2 streams a complete ID3v2.3 tag to w, writing nothing when there
// is nothing worth writing.
//
// The picture frame is streamed rather than assembled: its size is known from
// the image length, so a cover of any size costs one pass to the output file
// and no copy in between. The remaining frames are small — lyrics at their
// longest are tens of kilobytes — and are buffered only because the tag header
// has to state the total length before any of them are written.
func WriteID3v2(w io.Writer, t *Tags) error {
	if t == nil {
		return nil
	}

	var frames []byte
	addText := func(id, value string) {
		if value == "" {
			return
		}
		frames = appendNewFrame(frames, id, textFrame(value), id3v2Version)
	}

	addText(frameTitle, t.Title)
	addText(frameArtists, t.ArtistString())
	addText(frameAlbum, t.Album)
	if t.AlbumArtist != "" {
		addText(frameAlbumArtist, t.AlbumArtist)
	} else if len(t.Artists) > 0 {
		addText(frameAlbumArtist, t.Artists[0])
	}

	// Lyrics go in USLT, which is the frame players read for timed lyrics, and
	// again as user text. USLT is what the specification defines, but players
	// differ on where they look: the ones that ignore it read a LYRICS
	// user-text frame, which is the key the FLAC side writes and what most
	// taggers emit for MP3 too. Writing only USLT is a file whose lyrics half
	// the players cannot see.
	if t.Lyrics != "" {
		frames = appendNewFrame(frames, frameLyrics, usltFrame(t.Lyrics), id3v2Version)
	}
	// The individual timelines have no standard frame in the ID3 world, so
	// they ride along as user-defined text.
	addTXXX := func(description, value string) {
		if value == "" {
			return
		}
		frames = appendNewFrame(frames, frameUserText, txxxFrame(description, value), id3v2Version)
	}
	addTXXX("LYRICS", t.Lyrics)
	addTXXX("UNSYNCEDLYRICS", t.Lyrics)
	addTXXX("LYRICS_ORIGINAL", t.LyricsOriginal)
	addTXXX("LYRICS_TRANSLATION", t.LyricsTranslation)
	addTXXX("LYRICS_ROMANIZATION", t.LyricsRomanization)
	if t.MusicID != 0 {
		addTXXX("NETEASE_MUSIC_ID", strconv.FormatInt(t.MusicID, 10))
	}
	if t.AlbumID != 0 {
		addTXXX("NETEASE_ALBUM_ID", strconv.FormatInt(t.AlbumID, 10))
	}
	if ids := formatIDs(t.ArtistIDs); ids != "" {
		addTXXX("NETEASE_ARTIST_IDS", ids)
	}
	if t.Bitrate > 0 {
		addTXXX("NETEASE_BITRATE", strconv.Itoa(t.Bitrate))
	}

	picture := apicFrameSize(t.Cover, t.CoverMIME)
	if len(frames) == 0 && picture == 0 {
		return nil
	}

	body := len(frames) + picture + id3v2Padding
	head := make([]byte, 0, id3v2HeaderSize)
	head = append(head, 'I', 'D', '3', id3v2Version, 0, 0)
	head = appendSynchsafe(head, uint32(body))
	if _, err := w.Write(head); err != nil {
		return err
	}
	if _, err := w.Write(frames); err != nil {
		return err
	}
	if picture > 0 {
		if err := writeAPICFrame(w, t.Cover, t.CoverMIME); err != nil {
			return err
		}
	}
	_, err := w.Write(make([]byte, id3v2Padding))
	return err
}

// apicFrameSize returns the total size of the APIC frame for a cover, or zero
// when there is no cover to write.
func apicFrameSize(cover []byte, mime string) int {
	if len(cover) == 0 {
		return 0
	}
	return frameHeaderSize + len(apicBody(mime)) + len(cover)
}

// frameHeaderSize is the ten bytes every ID3v2.3 frame starts with: a
// four-character id, a four-byte size and two flag bytes.
const frameHeaderSize = 10

// appendFrame writes one frame into a tag of the given major version.
//
// The frame size is plain big-endian in 2.3 and synchsafe in 2.4. The two
// encodings agree only below 128 — a synchsafe size always reads *larger* when
// mistaken for a plain integer — so getting this wrong silently swallows the
// frames that follow. The flag bytes are the frame's own and are carried
// through untouched; they mean different things in the two versions, which is
// why a frame is only ever written back into the version it came from.
func appendFrame(dst []byte, fr id3Frame, version byte) []byte {
	dst = append(dst, fr.ID...)
	if version >= 4 {
		dst = appendSynchsafe(dst, uint32(len(fr.Body)))
	} else {
		dst = putUint32BE(dst, uint32(len(fr.Body)))
	}
	dst = append(dst, fr.Flags[0], fr.Flags[1])
	return append(dst, fr.Body...)
}

// appendNewFrame writes a freshly built frame, which carries no flags.
func appendNewFrame(dst []byte, id string, body []byte, version byte) []byte {
	return appendFrame(dst, id3Frame{ID: id, Body: body}, version)
}

// rebuildableVersion decides which tag version a rewrite should write, and
// refuses when a frame cannot be reproduced faithfully.
//
// A frame is only ever written back into the version it was read from: the size
// encoding differs between them, and so does the meaning of the flag bits, so a
// 2.4 frame re-emitted under a 2.3 header would be misread. A file with no tag
// at all gets a fresh 2.3 one, which is what this package has always written.
func rebuildableVersion(frames []id3Frame, tagSize int, version byte) (byte, error) {
	if tagSize < id3v2HeaderSize {
		return id3v2Version, nil
	}
	for _, fr := range frames {
		if !verifiesFrameBody(fr, version) {
			return version, ErrUnsupportedTag
		}
	}
	return version, nil
}

func writeFrameHeader(w io.Writer, id string, size int) error {
	head := make([]byte, 0, frameHeaderSize)
	head = append(head, id...)
	head = putUint32BE(head, uint32(size))
	head = append(head, 0, 0) // status and format flags
	_, err := w.Write(head)
	return err
}

// appendSynchsafe writes a 28-bit value with the high bit of each byte clear.
func appendSynchsafe(dst []byte, v uint32) []byte {
	return append(dst,
		byte(v>>21&0x7F),
		byte(v>>14&0x7F),
		byte(v>>7&0x7F),
		byte(v&0x7F),
	)
}

// textFrame encodes an ID3v2.3 text frame body.
func textFrame(value string) []byte {
	return append([]byte{1}, utf16LE(value)...) // 1 = UTF-16 with BOM
}

// txxxFrame encodes a user-defined text frame: a description and a value, each
// carrying its own byte order mark and separated by a UTF-16 terminator.
func txxxFrame(description, value string) []byte {
	body := []byte{1} // UTF-16 with BOM
	body = append(body, utf16LE(description)...)
	body = append(body, 0x00, 0x00) // descriptor terminator
	return append(body, utf16LE(value)...)
}

// usltFrame encodes an unsynchronised lyrics frame: encoding, a three-byte
// language code, a content descriptor, then the lyrics themselves.
func usltFrame(lyrics string) []byte {
	body := []byte{1}                           // UTF-16 with BOM
	body = append(body, "XXX"...)               // undefined language
	body = append(body, 0xFF, 0xFE, 0x00, 0x00) // empty descriptor, UTF-16 terminated
	return append(body, utf16LE(lyrics)...)
}

// writeAPICFrame writes an attached picture frame. The image itself is written
// straight from cover rather than assembled into a frame first.
func writeAPICFrame(w io.Writer, cover []byte, mime string) error {
	body := apicBody(mime)
	if err := writeFrameHeader(w, framePicture, len(body)+len(cover)); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	_, err := w.Write(cover)
	return err
}

// apicBody renders the fixed part of an APIC frame: everything ahead of the
// image. The description uses Latin-1 so it can simply be empty; the MIME type
// is always Latin-1 by definition.
func apicBody(mime string) []byte {
	if mime == "" {
		mime = "image/jpeg"
	}
	body := []byte{0} // Latin-1 for the description
	body = append(body, mime...)
	body = append(body, 0) // MIME terminator
	body = append(body, 3) // picture type 3 = front cover
	return append(body, 0) // empty description, terminated
}

// apicFrame renders an APIC frame body into memory, for the cover editor, which
// rebuilds whole frames from a tag it has already read.
func apicFrame(cover []byte, mime string) []byte {
	body := apicBody(mime)
	out := make([]byte, 0, len(body)+len(cover))
	out = append(out, body...)
	return append(out, cover...)
}

// ExistingID3v2Size returns the total length of an ID3v2 tag at the start of
// head, or zero when there is none.
//
// The size is read from the header alone, so it may exceed len(head); callers
// needing to skip the tag must supply a window at least this large. Dropping a
// source tag matters because the audio we decrypt often already carries one,
// and writing ours in front would leave two tags in the file.
func ExistingID3v2Size(head []byte) int {
	if len(head) < id3v2HeaderSize || string(head[:3]) != "ID3" {
		return 0
	}
	if head[3] == 0xFF || head[4] == 0xFF {
		return 0 // invalid version
	}
	size := int(head[6]&0x7F)<<21 | int(head[7]&0x7F)<<14 |
		int(head[8]&0x7F)<<7 | int(head[9]&0x7F)
	total := id3v2HeaderSize + size
	// A 2.4 footer sits outside the declared size, unlike the rest of the tag.
	if head[5]&0x10 != 0 {
		total += id3v2HeaderSize
	}
	return total
}
