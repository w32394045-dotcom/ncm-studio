package tag

import (
	"bytes"
	"errors"
	"io"
	"strconv"
)

// FLAC metadata block types we care about.
const (
	blockStreamInfo    = 0
	blockVorbisComment = 4
	blockPicture       = 6
)

// FLACSource describes where a decrypted FLAC stream's own metadata ends.
type FLACSource struct {
	// StreamInfo is the complete first metadata block, header included. It
	// holds the sample rate, channel count and total sample count, so it has
	// to be carried over verbatim into the rewritten file.
	StreamInfo []byte
	// FrameOffset is the index just past the source's metadata chain, where
	// the audio frames begin.
	FrameOffset int
}

// ErrNotFLAC reports a stream that does not start with the FLAC signature.
var ErrNotFLAC = errors.New("tag: not a FLAC stream")

// maxMetadataBlocks caps the walk so a malformed chain cannot spin forever.
const maxMetadataBlocks = 4096

// ParseFLACMetadata walks the metadata block chain at the start of buf.
//
// It returns more=true when buf ends before the chain does, which tells the
// caller to retry with a larger window; FLAC metadata is length-prefixed but
// has no total length, so the chain can only be measured by walking it.
func ParseFLACMetadata(buf []byte) (src *FLACSource, more bool, err error) {
	blocks, frameOffset, more, err := parseFLACBlocks(buf)
	if err != nil || more {
		return nil, more, err
	}
	src = &FLACSource{FrameOffset: frameOffset}
	// STREAMINFO is required to come first, so the leading block is it; the
	// header is rebuilt since only the payload was kept. Its is-last flag is
	// left clear, because a comment block always follows in the rewritten file.
	if len(blocks) > 0 {
		b := blocks[0]
		hdr := []byte{blockStreamInfo,
			byte(len(b.Data) >> 16), byte(len(b.Data) >> 8), byte(len(b.Data))}
		src.StreamInfo = append(hdr, b.Data...)
	}
	return src, false, nil
}

// BuildFLACMetadata renders the replacement metadata section: the signature,
// the source's STREAMINFO, our Vorbis comment, and the cover if there is one.
// Audio frames are appended by the caller immediately after this.
//
// It is the in-memory form of WriteFLACMetadata, for callers that want the
// bytes rather than a stream.
func BuildFLACMetadata(src *FLACSource, t *Tags) []byte {
	var buf bytes.Buffer
	// A bytes.Buffer cannot fail, so the only errors here are the ones this
	// function raises itself.
	if err := WriteFLACMetadata(&buf, src, t); err != nil {
		return nil
	}
	return buf.Bytes()
}

// WriteFLACMetadata streams the replacement metadata section to w.
//
// Streaming rather than returning a slice is what keeps the cover from being
// copied three times on its way to the file — a cover is easily a megabyte,
// and the file it is destined for is already open and sequential.
func WriteFLACMetadata(w io.Writer, src *FLACSource, t *Tags) error {
	streamInfo := src.StreamInfo
	if len(streamInfo) < 4 {
		// No usable STREAMINFO: emit an empty one rather than a malformed
		// header. The file will not decode, but it stays structurally valid.
		streamInfo = []byte{byte(blockStreamInfo), 0, 0, 34}
		streamInfo = append(streamInfo, make([]byte, 34)...)
	}

	if _, err := w.Write([]byte("fLaC")); err != nil {
		return err
	}

	// STREAMINFO first, with its is-last flag cleared: it can never be last
	// once a comment block follows it.
	si := make([]byte, len(streamInfo))
	copy(si, streamInfo)
	si[0] &= 0x7F
	if _, err := w.Write(si); err != nil {
		return err
	}

	comment := buildVorbisComment(t)
	if err := writeBlock(w, blockVorbisComment, comment, len(t.Cover) == 0); err != nil {
		return err
	}
	if len(t.Cover) > 0 {
		return writePictureBlock(w, t.Cover, t.CoverMIME)
	}
	return nil
}

// writeBlock writes a metadata block header and payload. The final block in
// the chain must carry the is-last flag; a file without it will not play.
func writeBlock(w io.Writer, blockType byte, payload []byte, isLast bool) error {
	if err := writeBlockHeader(w, blockType, len(payload), isLast); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func writeBlockHeader(w io.Writer, blockType byte, size int, isLast bool) error {
	first := blockType
	if isLast {
		first |= 0x80
	}
	_, err := w.Write([]byte{first, byte(size >> 16), byte(size >> 8), byte(size)})
	return err
}

// buildVorbisComment renders the Vorbis comment block, which is where a FLAC
// file carries all of its textual metadata.
func buildVorbisComment(t *Tags) []byte {
	var fields []string
	add := func(key, value string) {
		if value != "" {
			fields = append(fields, key+"="+value)
		}
	}

	add("TITLE", t.Title)
	// Vorbis comments repeat the key once per value rather than joining, so
	// each credited artist stays a separate entry and players that list
	// artists individually get them all.
	for _, a := range t.Artists {
		add("ARTIST", a)
	}
	add("ALBUM", t.Album)
	if t.AlbumArtist != "" {
		add("ALBUMARTIST", t.AlbumArtist)
	} else if len(t.Artists) > 0 {
		add("ALBUMARTIST", t.Artists[0])
	}

	// Both spellings are written because players disagree about which to
	// read, and the duplicated few kilobytes are irrelevant next to the audio.
	add("LYRICS", t.Lyrics)
	add("UNSYNCEDLYRICS", t.Lyrics)
	add("LYRICS_ORIGINAL", t.LyricsOriginal)
	add("LYRICS_TRANSLATION", t.LyricsTranslation)
	add("LYRICS_ROMANIZATION", t.LyricsRomanization)

	if t.MusicID != 0 {
		add("NETEASE_MUSIC_ID", strconv.FormatInt(t.MusicID, 10))
	}
	if t.AlbumID != 0 {
		add("NETEASE_ALBUM_ID", strconv.FormatInt(t.AlbumID, 10))
	}
	if ids := formatIDs(t.ArtistIDs); ids != "" {
		add("NETEASE_ARTIST_IDS", ids)
	}
	if t.Bitrate > 0 {
		add("NETEASE_BITRATE", strconv.Itoa(t.Bitrate))
	}

	const vendor = "ncm-studio"
	out := make([]byte, 0, 256)
	out = putUint32LE(out, uint32(len(vendor)))
	out = append(out, vendor...)
	out = putUint32LE(out, uint32(len(fields)))
	for _, f := range fields {
		out = putUint32LE(out, uint32(len(f)))
		out = append(out, f...)
	}
	return out
}

// writePictureBlock writes a PICTURE block holding the cover art.
//
// Only the small fixed-size header is built in memory; the image goes to the
// writer as it is. The block's length has to precede it, which is why the head
// is measured rather than the block.
func writePictureBlock(w io.Writer, cover []byte, mime string) error {
	head := pictureHead(cover, mime)
	if err := writeBlockHeader(w, blockPicture, len(head)+len(cover), true); err != nil {
		return err
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	_, err := w.Write(cover)
	return err
}

// pictureHead renders everything in a PICTURE block except the image itself.
func pictureHead(cover []byte, mime string) []byte {
	if mime == "" {
		mime = "image/jpeg"
	}
	const description = ""
	var head []byte
	head = putUint32BE(head, 3) // picture type 3 = front cover
	head = putUint32BE(head, uint32(len(mime)))
	head = append(head, mime...)
	head = putUint32BE(head, uint32(len(description)))
	head = append(head, description...)
	// Dimensions, colour depth and palette size are optional in practice and
	// expensive to compute; zero means "unknown" and players read the image.
	for range 4 {
		head = putUint32BE(head, 0)
	}
	return putUint32BE(head, uint32(len(cover)))
}

// buildPicture renders a PICTURE block payload into memory.
//
// The bulk decrypt path streams instead — see writePictureBlock — but the cover
// editor rebuilds blocks it has already read from the file, so it wants bytes.
func buildPicture(cover []byte, mime string) []byte {
	head := pictureHead(cover, mime)
	out := make([]byte, 0, len(head)+len(cover))
	out = append(out, head...)
	return append(out, cover...)
}
