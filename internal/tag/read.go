package tag

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf16"
)

// AudioFormat identifies a container this package can rewrite in place.
type AudioFormat string

const (
	FormatFLAC AudioFormat = "flac"
	FormatMP3  AudioFormat = "mp3"
)

// fingerprintBytes is how much of the audio payload is digested when
// identifying a track. It matches the store's source fingerprint: enough of the
// stream to be distinctive, without reading a whole 150 MB file.
const fingerprintBytes = 256 << 10

// maxMetadataBytes caps how much of a file's metadata prefix will be buffered
// while looking for a cover. Album art in these files runs to a few megabytes;
// anything past this is a malformed chain, not a large picture.
const maxMetadataBytes = 64 << 20

// ErrUnsupportedFormat reports a file that is neither FLAC nor MP3.
var ErrUnsupportedFormat = errors.New("tag: not a FLAC or MP3 file")

// CoverArt is a picture pulled out of an audio file.
type CoverArt struct {
	Data []byte
	MIME string
}

// flacBlock is one metadata block, payload only. The block header is rebuilt
// when the chain is written back out.
type flacBlock struct {
	Type byte
	Data []byte
}

// AudioOffset reports the container format and the byte offset where the audio
// payload begins — past everything this package rewrites.
//
// It reads only the metadata prefix, so it is cheap enough to call before
// deciding whether a file is worth touching.
func AudioOffset(path string) (AudioFormat, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	head := make([]byte, 10)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", 0, err
	}
	head = head[:n]

	switch {
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		blocks, _, _, err := readFLACBlocks(f)
		if err != nil {
			return "", 0, err
		}
		var offset int64 = 4
		for _, b := range blocks {
			offset += 4 + int64(len(b.Data))
		}
		return FormatFLAC, offset, nil

	case len(head) >= 3 && string(head[:3]) == "ID3":
		return FormatMP3, int64(ExistingID3v2Size(head)), nil

	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		// No tag at all; the file opens on a frame sync.
		return FormatMP3, 0, nil
	}
	return "", 0, ErrUnsupportedFormat
}

// AudioFingerprint identifies a track by its audio payload alone.
//
// This is deliberately not a hash of the file: the whole point is that it
// survives operations that rewrite the metadata prefix. Replacing the cover
// changes the file's bytes, its size and its metadata offset, but not one byte
// of the audio, so a digest taken from the payload is stable across cover edits
// and across renames — which is what lets a cover library stay attached to its
// track.
func AudioFingerprint(path string) (string, error) {
	format, offset, err := AudioOffset(path)
	if err != nil {
		return "", err
	}
	_ = format

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	audioLen := fi.Size() - offset
	if audioLen < 0 {
		return "", fmt.Errorf("tag: %s: audio offset %d is past the end of the file", path, offset)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}

	// The length is folded in so that two tracks sharing an opening riff do not
	// collide, and so a truncated file is never mistaken for the original.
	h := sha256.New()
	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], uint64(audioLen))
	h.Write(sizeBuf[:])
	if _, err := io.Copy(h, io.LimitReader(f, fingerprintBytes)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadCover returns the front cover embedded in an audio file, or nil when
// there is none.
func ReadCover(path string) (*CoverArt, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	head := make([]byte, 10)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	head = head[:n]

	switch {
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		blocks, _, _, err := readFLACBlocks(f)
		if err != nil {
			return nil, err
		}
		return coverFromFLAC(blocks), nil

	case len(head) >= 3 && string(head[:3]) == "ID3":
		return coverFromID3(readID3FramesLenient(f, head)), nil
	}
	return nil, ErrUnsupportedFormat
}

// ReadMusicID returns the NetEase song id carried by a file written by this
// tool, or 0 when the file does not carry one.
func ReadMusicID(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	head := make([]byte, 10)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return 0
	}
	head = head[:n]

	switch {
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		blocks, _, _, err := readFLACBlocks(f)
		if err != nil {
			return 0
		}
		for _, b := range blocks {
			if b.Type == blockVorbisComment {
				if v, ok := vorbisValue(b.Data, "NETEASE_MUSIC_ID"); ok {
					return parseInt64(v)
				}
			}
		}
	case len(head) >= 3 && string(head[:3]) == "ID3":
		for _, fr := range readID3FramesLenient(f, head) {
			if fr.ID == frameUserText {
				if desc, value, ok := parseTXXX(fr.Body); ok && desc == "NETEASE_MUSIC_ID" {
					return parseInt64(value)
				}
			}
		}
	}
	return 0
}

func coverFromFLAC(blocks []flacBlock) *CoverArt {
	var fallback *CoverArt
	for _, b := range blocks {
		if b.Type != blockPicture {
			continue
		}
		data, mime, picType, ok := parsePicture(b.Data)
		if !ok {
			continue
		}
		art := &CoverArt{Data: data, MIME: mime}
		if picType == 3 { // front cover wins over anything else
			return art
		}
		if fallback == nil {
			fallback = art
		}
	}
	return fallback
}

// parsePicture decodes a FLAC PICTURE block.
//
// The layout mixes the two conventions: the MIME type, description and image
// data are length-prefixed, but the picture type and the four dimension fields
// are bare big-endian integers. Reading the bare ones as if they carried a
// length silently consumes the fields that follow.
func parsePicture(b []byte) (data []byte, mime string, picType uint32, ok bool) {
	if len(b) < 4 {
		return nil, "", 0, false
	}
	picType = binary.BigEndian.Uint32(b[:4])
	b = b[4:]

	// readField consumes one length-prefixed field.
	readField := func() ([]byte, bool) {
		if len(b) < 4 {
			return nil, false
		}
		n := int(binary.BigEndian.Uint32(b[:4]))
		b = b[4:]
		if n < 0 || n > len(b) {
			return nil, false
		}
		v := b[:n]
		b = b[n:]
		return v, true
	}

	m, ok := readField()
	if !ok {
		return nil, "", 0, false
	}
	if _, ok := readField(); !ok { // description
		return nil, "", 0, false
	}
	// width, height, colour depth, palette size
	if len(b) < 16 {
		return nil, "", 0, false
	}
	b = b[16:]

	d, ok := readField()
	if !ok || len(d) == 0 {
		return nil, "", 0, false
	}
	return d, sniffMIME(string(m), d), picType, true
}

func coverFromID3(frames []id3Frame) *CoverArt {
	var fallback *CoverArt
	for _, fr := range frames {
		if fr.ID != framePicture {
			continue
		}
		data, mime, picType, ok := parseAPIC(fr.Body)
		if !ok {
			continue
		}
		art := &CoverArt{Data: data, MIME: mime}
		if picType == 3 {
			return art
		}
		if fallback == nil {
			fallback = art
		}
	}
	return fallback
}

// parseAPIC decodes an ID3v2 attached-picture frame.
func parseAPIC(b []byte) (data []byte, mime string, picType byte, ok bool) {
	if len(b) < 4 {
		return nil, "", 0, false
	}
	enc := b[0]
	b = b[1:]

	end := indexByte(b, 0)
	if end < 0 {
		return nil, "", 0, false
	}
	mime = string(b[:end])
	b = b[end+1:]

	if len(b) < 1 {
		return nil, "", 0, false
	}
	picType = b[0]
	b = b[1:]

	// The description is terminated according to the body's text encoding:
	// two zero bytes for UTF-16, one otherwise.
	switch enc {
	case 1, 2:
		i := 0
		for i+1 < len(b) {
			if b[i] == 0 && b[i+1] == 0 {
				break
			}
			i += 2
		}
		if i+1 >= len(b) {
			return nil, "", 0, false
		}
		b = b[i+2:]
	default:
		i := indexByte(b, 0)
		if i < 0 {
			return nil, "", 0, false
		}
		b = b[i+1:]
	}

	if len(b) == 0 {
		return nil, "", 0, false
	}
	return b, sniffMIME(mime, b), picType, true
}

// sniffMIME falls back to the image's own magic bytes when the declared type is
// missing or wrong, which is common in tags written by other tools.
func sniffMIME(declared string, data []byte) string {
	if declared != "" && declared != "image/" && !strings.HasPrefix(declared, "-->") {
		return declared
	}
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	}
	if declared != "" {
		return declared
	}
	return "image/jpeg"
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// ── metadata prefix readers ───────────────────────────────────────────────

// readFLACBlocks walks the metadata chain and returns every block.
//
// The chain is length-prefixed but has no total length, so it is buffered
// progressively: the window doubles until the walk reaches the last block.
func readFLACBlocks(f *os.File) ([]flacBlock, int, bool, error) {
	window := 1 << 20
	for {
		// Rewound every time, not once before the loop: a retry that resumed
		// where the last read stopped would hand the parser a slice of audio
		// frames and be told the file is not a FLAC stream at all. The window
		// only grows when a real chain did not fit, which is exactly the case a
		// large embedded cover produces.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, 0, false, err
		}
		if window > maxMetadataBytes {
			return nil, 0, false, errors.New("tag: FLAC metadata chain is implausibly large")
		}
		buf := make([]byte, window)
		n, err := io.ReadFull(f, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, 0, false, err
		}
		buf = buf[:n]

		blocks, frameOffset, more, err := parseFLACBlocks(buf)
		if err != nil {
			return nil, 0, false, err
		}
		if !more {
			return blocks, frameOffset, false, nil
		}
		window *= 4
	}
}

// parseFLACBlocks walks as much of the chain as buf holds. more=true means the
// buffer ended first and the caller should retry with a larger window.
func parseFLACBlocks(buf []byte) (blocks []flacBlock, frameOffset int, more bool, err error) {
	if len(buf) < 4 || string(buf[:4]) != "fLaC" {
		return nil, 0, false, ErrNotFLAC
	}
	pos := 4
	for range maxMetadataBlocks {
		if pos+4 > len(buf) {
			return nil, 0, true, nil
		}
		header := buf[pos : pos+4]
		isLast := header[0]&0x80 != 0
		length := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
		end := pos + 4 + length
		if end > len(buf) {
			return nil, 0, true, nil
		}
		payload := make([]byte, length)
		copy(payload, buf[pos+4:end])
		blocks = append(blocks, flacBlock{Type: header[0] & 0x7F, Data: payload})
		pos = end
		if isLast {
			return blocks, pos, false, nil
		}
	}
	return nil, 0, false, errors.New("tag: FLAC metadata chain is implausibly long")
}

// id3Frame is one ID3v2 frame: its id, its two flag bytes and its body.
//
// The flags are carried rather than dropped because some of them describe how
// to read the body — a compression or data-length-indicator bit means the bytes
// are not what a caller would otherwise assume — so a frame copied without them
// is a frame corrupted.
type id3Frame struct {
	ID    string
	Flags [2]byte
	Body  []byte
}

// ErrUnsupportedTag reports an ID3 tag this package cannot round-trip: a
// version it does not parse, unsynchronised frames, or an extended header. The
// read path treats it as "no frames found", but the write path must refuse,
// because a tag rebuilt from frames we failed to read loses the ones we did not
// understand.
var ErrUnsupportedTag = errors.New("tag: unsupported ID3 tag")

// readID3Frames reads the whole ID3v2 tag at the head of f, returning the
// frames, the tag's total length in the file, and the major version they were
// read from.
//
// The version is returned because a frame can only be written back correctly
// into the version it came from: the size encoding differs, and so does the
// meaning of the flag bits.
func readID3Frames(f *os.File, head []byte) ([]id3Frame, int, byte, error) {
	size := ExistingID3v2Size(head)
	if size < id3v2HeaderSize {
		return nil, 0, 0, nil
	}
	if size > maxMetadataBytes {
		return nil, 0, 0, errors.New("tag: ID3v2 tag is implausibly large")
	}
	version := head[3]
	// 2.2 uses three-character frame ids and three-byte sizes, and both header
	// flags below mean the body is not a plain frame list. Guessing at either
	// yields a tag that parses to nothing, and a caller that then writes what it
	// read would drop every frame the file had.
	if version != 3 && version != 4 {
		return nil, size, version, ErrUnsupportedTag
	}
	if head[5]&0xC0 != 0 { // unsynchronisation or extended header
		return nil, size, version, ErrUnsupportedTag
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, 0, 0, err
	}
	frames, clean, err := parseFrameList(buf[id3v2HeaderSize:], version)
	if err != nil {
		return nil, size, version, err
	}
	if !clean {
		// The walk stopped somewhere other than the padding that should follow
		// the last frame, so it did not understand this tag. Returning the
		// frames it did read would let a rewrite write back a tag missing
		// everything it failed to parse.
		return nil, size, version, ErrUnsupportedTag
	}
	return frames, size, version, nil
}

// readID3FramesLenient reads frames for callers that only want to look
// something up. A tag this package cannot parse yields no frames rather than an
// error: there is nothing to find in it either way, and a caller asking whether
// a file has a cover should not have to handle a parse failure to hear "no".
func readID3FramesLenient(f *os.File, head []byte) []id3Frame {
	frames, _, _, err := readID3Frames(f, head)
	if err != nil {
		return nil
	}
	return frames
}

// parseFrameList walks the frames inside a tag body — that is, past the 10-byte
// tag header — stopping at the padding that follows the last frame.
//
// clean reports whether the walk ended the way a well-formed tag does: at
// padding that runs to the end of the declared size. Anything else means the
// walk stopped early, which is the signature of a tag this parser does not
// understand — and a caller that rewrites tags must not treat a partial read as
// the whole truth.
func parseFrameList(body []byte, version byte) (frames []id3Frame, clean bool, err error) {
	var out []id3Frame
	pos := 0
	for pos+frameHeaderSize <= len(body) {
		id := string(body[pos : pos+4])
		if !validFrameID(id) {
			break // padding begins here
		}
		// 2.3 frame sizes are plain big-endian; 2.4 uses synchsafe. Reading a
		// 2.4 size as 2.3 inflates it — the two encodings agree only below 128
		// — which runs the walk past the frames that follow and swallows them.
		var size int
		if version >= 4 {
			size = synchsafeUint32(body[pos+4 : pos+8])
		} else {
			size = int(binary.BigEndian.Uint32(body[pos+4 : pos+8]))
		}
		if size < 0 || pos+frameHeaderSize+size > len(body) {
			return out, false, nil // a frame that runs off the end
		}
		var flags [2]byte
		copy(flags[:], body[pos+8:pos+10])
		payload := make([]byte, size)
		copy(payload, body[pos+frameHeaderSize:pos+frameHeaderSize+size])
		out = append(out, id3Frame{ID: id, Flags: flags, Body: payload})
		pos += frameHeaderSize + size
	}
	return out, allZero(body[pos:]), nil
}

// allZero reports whether every byte is zero, which is what ID3 padding is.
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// synchsafeUint32 decodes a 28-bit value stored seven bits per byte.
func synchsafeUint32(b []byte) int {
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 |
		int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

// verifiesFrameBody reports whether a frame's format flags leave its body as
// plain bytes that can be copied verbatim.
//
// The status flags in Flags[0] describe the frame rather than its encoding and
// are always safe to carry through. The format flags in Flags[1] are not:
// compression, encryption, grouping, unsynchronisation and the 2.4
// data-length-indicator all change what the body bytes are, and none of them
// survives being copied into a freshly built tag.
func verifiesFrameBody(fr id3Frame, version byte) bool {
	if version >= 4 {
		// 0x40 grouping, 0x08 compression, 0x04 encryption,
		// 0x02 unsynchronisation, 0x01 data length indicator.
		return fr.Flags[1]&0x4F == 0
	}
	// 0x80 compression, 0x40 encryption, 0x20 grouping.
	return fr.Flags[1]&0xE0 == 0
}

func validFrameID(id string) bool {
	if len(id) != 4 {
		return false
	}
	for i := range 4 {
		c := id[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// ── tag value readers ─────────────────────────────────────────────────────

// vorbisValue looks up one key in a Vorbis comment block.
func vorbisValue(block []byte, key string) (string, bool) {
	if len(block) < 4 {
		return "", false
	}
	vendorLen := int(binary.LittleEndian.Uint32(block[:4]))
	pos := 4 + vendorLen
	if pos+4 > len(block) {
		return "", false
	}
	count := int(binary.LittleEndian.Uint32(block[pos : pos+4]))
	pos += 4

	for range count {
		if pos+4 > len(block) {
			return "", false
		}
		n := int(binary.LittleEndian.Uint32(block[pos : pos+4]))
		pos += 4
		if n < 0 || pos+n > len(block) {
			return "", false
		}
		entry := string(block[pos : pos+n])
		pos += n
		if eq, value, ok := strings.Cut(entry, "="); ok && strings.EqualFold(eq, key) {
			return value, true
		}
	}
	return "", false
}

// parseTXXX splits a user-defined text frame into its description and value.
func parseTXXX(body []byte) (description, value string, ok bool) {
	if len(body) < 1 {
		return "", "", false
	}
	enc := body[0]
	rest := body[1:]
	isUTF16 := enc == 1 || enc == 2

	split := -1
	if isUTF16 {
		for i := 0; i+1 < len(rest); i += 2 {
			if rest[i] == 0 && rest[i+1] == 0 {
				split = i
				break
			}
		}
		if split < 0 {
			return "", "", false
		}
		description = decodeID3Text(enc, rest[:split])
		return description, decodeID3Text(enc, rest[split+2:]), true
	}
	split = indexByte(rest, 0)
	if split < 0 {
		return "", "", false
	}
	return decodeID3Text(enc, rest[:split]), decodeID3Text(enc, rest[split+1:]), true
}

// decodeTextFrame decodes a whole text-frame body, whose first byte states how
// the text after it is encoded.
func decodeTextFrame(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return decodeID3Text(body[0], body[1:])
}

// parseUSLT extracts the lyrics text from an unsynchronised-lyrics frame.
//
// The body is not a plain text frame: after the encoding byte come three bytes
// of language and a NUL-terminated content descriptor, and only then the
// timeline. Decoding it with decodeTextFrame would leave the descriptor stuck
// to the front of the lyrics — and, worse, would make a file look as though its
// lyrics had changed on every pass, so each backfill run would rewrite it.
func parseUSLT(body []byte) string {
	if len(body) < 4 {
		return ""
	}
	enc := body[0]
	rest := body[4:] // past the encoding byte and the language code

	if enc == 1 || enc == 2 { // UTF-16: the descriptor ends on a two-byte NUL
		for i := 0; i+1 < len(rest); i += 2 {
			if rest[i] == 0 && rest[i+1] == 0 {
				return decodeID3Text(enc, rest[i+2:])
			}
		}
		return ""
	}
	if i := indexByte(rest, 0); i >= 0 {
		return decodeID3Text(enc, rest[i+1:])
	}
	return ""
}

// decodeID3Text decodes an ID3v2 text payload. Non-Latin text is stored as
// UTF-16 with a byte order mark, so the BOM decides the byte order.
func decodeID3Text(enc byte, b []byte) string {
	switch enc {
	case 0: // Latin-1
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		return string(runes)
	case 1: // UTF-16 with BOM
		if len(b) < 2 {
			return ""
		}
		big := b[0] == 0xFE && b[1] == 0xFF
		le := b[0] == 0xFF && b[1] == 0xFE
		if big || le {
			b = b[2:]
		}
		units := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if big {
				units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				units = append(units, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}
		return string(utf16.Decode(units))
	case 2: // UTF-16BE without BOM
		units := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		}
		return string(utf16.Decode(units))
	default: // 3 = UTF-8
		return string(b)
	}
}

func parseInt64(s string) int64 {
	var n int64
	seen := false
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
		seen = true
	}
	if !seen {
		return 0
	}
	return n
}
