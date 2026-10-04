package tag

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Lyrics is the set of timelines written into a file's tags.
//
// The field names mirror the keys buildVorbisComment already writes, so a file
// backfilled by this path is indistinguishable from one the decryptor produced
// with lyrics in the first place. Merged is the original with any translation
// interleaved, which is what plain-lyrics players read.
type Lyrics struct {
	Merged       string
	Original     string
	Translation  string
	Romanization string

	// TranslationSource and TranslationModel say who produced the translation
	// when it was not a person. A machine translation is worth having and worth
	// labelling: a reader who finds one in a file should be able to tell it from
	// a curated translation, and a later run should be able to replace its own
	// output without wondering whose work it is discarding.
	//
	// They sit outside Equal on purpose. They say where the words came from,
	// not what they are, so a file whose words are already right is not
	// rewritten to change a note about them.
	TranslationSource string
	TranslationModel  string
}

// Empty reports whether there is nothing to write.
func (l Lyrics) Empty() bool {
	return l.Merged == "" && l.Original == "" && l.Translation == "" && l.Romanization == ""
}

// MachineTranslated reports whether the translation in this file was written by
// a model rather than a person.
func (l Lyrics) MachineTranslated() bool { return l.TranslationSource != "" }

// filled applies the rules a write uses, so a comparison sees the values that
// would actually land in the file rather than the caller's shorthand.
func (l Lyrics) filled() Lyrics {
	l.Merged = strings.TrimSpace(l.Merged)
	l.Original = strings.TrimSpace(l.Original)
	l.Translation = strings.TrimSpace(l.Translation)
	l.Romanization = strings.TrimSpace(l.Romanization)
	if l.Merged == "" {
		// A caller that only has the original still means to write lyrics;
		// leaving Merged empty would silently write nothing at all.
		l.Merged = l.Original
	}
	return l
}

// Equal reports whether writing o would change anything. It is what keeps a
// second backfill pass over a finished library cheap: the lyrics are already
// there, so the hundred-megabyte rewrite is skipped rather than repeated.
func (l Lyrics) Equal(o Lyrics) bool {
	a, b := l.filled(), o.filled()
	return a.Merged == b.Merged && a.Original == b.Original &&
		a.Translation == b.Translation && a.Romanization == b.Romanization
}

// Credit is what the catalogue says a track is, once a match has been
// confirmed: the name a file may be missing even when it already carries the
// lyrics.
//
// A file with no title and no artist is one a player cannot name, and a player
// that cannot name a track has nothing to attach the lyrics to — its own
// lookup, and any online match it might do, both start from the song's name.
// So the moment a match is confirmed is the moment to write the identity in,
// because that is when it is known for free.
//
// Filling is one-way: a value the file already carries is never replaced. The
// file's own tags are the user's, and a search result is only ever a guess.
type Credit struct {
	MusicID int64
	Title   string
	Artists []string
	Album   string
}

// Fills reports whether this credit has a name the file is missing.
func (c Credit) Fills(info *AudioInfo) bool {
	return (c.Title != "" && strings.TrimSpace(info.Title) == "") ||
		(len(c.Artists) > 0 && len(info.Artists) == 0) ||
		(c.Album != "" && strings.TrimSpace(info.Album) == "")
}

// hasName reports whether the credit carries anything to identify a track by.
// The id alone does not count: it is a number no player shows and no player
// searches by.
func (c Credit) hasName() bool {
	return c.Title != "" || len(c.Artists) > 0 || c.Album != ""
}

// SetLyrics writes lyrics into an existing audio file, in place.
//
// The file is rewritten by copying every metadata block or frame through
// untouched and replacing only the ones that carry lyrics. That is deliberately
// not the same as re-encoding the tag from a Tags value: a rebuild would drop
// everything this package does not model — replay gain, composer, the encoder
// line, whatever else a tagger left behind — and silently discarding a user's
// metadata is a worse failure than not writing lyrics at all.
//
// The audio payload is streamed through rather than re-encoded, and the result
// is written beside the original and renamed over it, so an interrupted write
// leaves the original intact.
// The error is a named result so that the deferred cleanup below can see it.
// Without that, a refusal — a file this package cannot parse — returns before
// the temporary has been removed and leaves a .part beside the user's audio.
func SetLyrics(path string, l Lyrics, credit Credit, onProgress ProgressFunc) (err error) {
	l = l.filled()
	if l.Empty() && !credit.hasName() {
		// A write with neither lyrics nor a name would rewrite the whole file
		// to say nothing at all.
		return errors.New("tag: no lyrics to write")
	}

	// Concurrent rewrites of one file interleave; see lock.go.
	release := lockRewrite(path)
	defer release()

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	total := fi.Size()

	head := make([]byte, 10)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	head = head[:n]

	tmp := path + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(tmp)
		}
	}()

	switch {
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		err = rewriteFLACLyrics(f, out, l, credit, total, onProgress)
	case len(head) >= 3 && string(head[:3]) == "ID3":
		err = rewriteMP3Lyrics(f, out, head, l, credit, total, onProgress)
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		err = rewriteMP3Lyrics(f, out, nil, l, credit, total, onProgress)
	default:
		return ErrUnsupportedFormat
	}
	if err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lyricKeys are the comment keys a lyrics rewrite owns. Anything else in the
// block is somebody else's and is carried through.
var lyricKeys = []string{
	"LYRICS",
	"UNSYNCEDLYRICS",
	"LYRICS_ORIGINAL",
	"LYRICS_TRANSLATION",
	"LYRICS_ROMANIZATION",
	"LYRICS_TRANSLATION_SOURCE",
	"LYRICS_TRANSLATION_MODEL",
}

func applyLyrics(c *vorbisComments, l Lyrics, credit Credit) {
	c.set("LYRICS", l.Merged)
	c.set("UNSYNCEDLYRICS", l.Merged)
	c.set("LYRICS_ORIGINAL", l.Original)
	c.set("LYRICS_TRANSLATION", l.Translation)
	c.set("LYRICS_ROMANIZATION", l.Romanization)
	// Who wrote the translation; see Lyrics. Written as a pair with the
	// translation itself, so a file that has no translation is not labelled as
	// though it had one.
	if l.Translation != "" {
		c.set("LYRICS_TRANSLATION_SOURCE", l.TranslationSource)
		c.set("LYRICS_TRANSLATION_MODEL", l.TranslationModel)
	}
	if credit.MusicID != 0 {
		c.set("NETEASE_MUSIC_ID", strconv.FormatInt(credit.MusicID, 10))
	}
	// The identity a player needs before it will look the lyrics up; see
	// Credit. Each field is filled only when the file has nothing there, and a
	// field that is present but blank is overwritten in place rather than
	// doubled.
	if !c.has("TITLE") {
		c.set("TITLE", credit.Title)
	}
	if !c.has("ARTIST") {
		first := true
		for _, a := range credit.Artists {
			if a == "" {
				continue
			}
			if first {
				c.set("ARTIST", a)
				first = false
				continue
			}
			c.add("ARTIST", a)
		}
	}
	if !c.has("ALBUM") {
		c.set("ALBUM", credit.Album)
	}
}

func rewriteFLACLyrics(src, dst *os.File, l Lyrics, credit Credit, total int64, onProgress ProgressFunc) error {
	// The picture is not read into memory here: it is neither edited nor
	// examined by this function, and on a file with a megabyte of embedded art
	// that is a megabyte of copying per track. What it does need is the block
	// list, which is a few kilobytes.
	blocks, frameOffset, _, err := readFLACBlocks(src, false)
	if err != nil {
		return err
	}

	// Build the new comment block from the one already present so unknown keys
	// survive; a file that has none gets a fresh block right after STREAMINFO,
	// which is where the specification expects to find it.
	comments := &vorbisComments{vendor: "ncm-studio"}
	replaced := false
	var out []flacBlock
	for _, b := range blocks {
		if b.Type == blockVorbisComment {
			if replaced {
				continue // a second comment block would shadow the first
			}
			existing, _, err := parseVorbisComments(b.Data)
			if err != nil {
				return err
			}
			comments = existing
			replaced = true
		}
		out = append(out, b)
	}
	applyLyrics(comments, l, credit)

	block := flacBlock{Type: blockVorbisComment, Data: comments.marshal()}
	if !replaced {
		// Insert directly after STREAMINFO, or at the head if this file somehow
		// has no STREAMINFO for it to follow.
		at := 0
		if len(out) > 0 && out[0].Type == blockStreamInfo {
			at = 1
		}
		out = append(out, flacBlock{})
		copy(out[at+1:], out[at:])
		out[at] = block
	} else {
		for i := range out {
			if out[i].Type == blockVorbisComment {
				out[i] = block
				break
			}
		}
	}

	if _, err := dst.WriteString("fLaC"); err != nil {
		return err
	}
	for i, b := range out {
		if err := writeBlockFrom(dst, src, b, i == len(out)-1); err != nil {
			return err
		}
	}

	if _, err := src.Seek(int64(frameOffset), io.SeekStart); err != nil {
		return err
	}
	return copyAudio(src, dst, total, onProgress)
}

// writeBlockFrom writes one metadata block, taking its payload from Memory when
// the reader kept it and from the source file when it did not.
//
// The streaming case is the picture: a chain walked without buffering artwork
// still has to be written back with it, and copying it straight from the source
// to the destination costs a buffer instead of a megabyte. The read is short by
// construction — Size came from the block's own header — so io.ReadFull detects
// a file that changed underneath us rather than writing a short block.
func writeBlockFrom(dst, src *os.File, b flacBlock, isLast bool) error {
	if b.Data == nil && b.Size > 0 {
		if err := writeBlockHeader(dst, b.Type, b.Size, isLast); err != nil {
			return err
		}
		if _, err := src.Seek(b.At, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(dst, src, int64(b.Size)); err != nil {
			return err
		}
		return nil
	}
	return writeBlock(dst, b.Type, b.Data, isLast)
}

func rewriteMP3Lyrics(src, dst *os.File, head []byte, l Lyrics, credit Credit, total int64, onProgress ProgressFunc) error {
	frames, tagSize, version, err := readID3Frames(src, head)
	if err != nil {
		return err
	}
	version, err = rebuildableVersion(frames, tagSize, version)
	if err != nil {
		return err
	}

	// Drop the frames this rewrite owns and keep every other one verbatim,
	// which is what preserves composer, album art and anything written by a
	// different tagger.
	kept := make([]id3Frame, 0, len(frames)+5)
	for _, fr := range frames {
		if fr.ID == frameLyrics {
			continue
		}
		if fr.ID == frameUserText {
			if desc, value, ok := parseTXXX(fr.Body); ok {
				if strings.EqualFold(desc, "NETEASE_MUSIC_ID") {
					// The id frame is rewritten, not simply dropped, so an
					// existing id survives a write that does not carry one —
					// otherwise translating a track's lyrics would cost it the
					// one tag that says which track it is.
					if credit.MusicID == 0 {
						credit.MusicID = parseInt64(value)
					}
					continue
				}
				if isLyricKey(desc) {
					continue
				}
			}
		}
		kept = append(kept, fr)
	}
	// The same one-way identity fill as the FLAC side; see Credit. Each field
	// is looked up among the frames kept above, so a file that already names
	// itself keeps its own spelling, and a frame that is present but blank is
	// filled in rather than joined by a second one.
	fill := func(id, value string) {
		if value == "" {
			return
		}
		for i, fr := range kept {
			if fr.ID != id {
				continue
			}
			if strings.TrimSpace(decodeTextFrame(fr.Body)) != "" {
				return
			}
			kept[i] = id3Frame{ID: id, Body: textFrame(value)}
			return
		}
		kept = append(kept, id3Frame{ID: id, Body: textFrame(value)})
	}
	fill(frameTitle, credit.Title)
	// ID3 has one artist frame holding the whole credit, so the names are
	// joined with the same separator this package writes on the decrypt path.
	fill(frameArtists, strings.Join(credit.Artists, "; "))
	fill(frameAlbum, credit.Album)

	kept = append(kept, id3Frame{ID: frameLyrics, Body: usltFrame(l.Merged)})
	add := func(desc, value string) {
		if value != "" {
			kept = append(kept, id3Frame{ID: frameUserText, Body: txxxFrame(desc, value)})
		}
	}
	// The merged timeline goes in twice on purpose. USLT is the frame the
	// specification defines for it, but players differ on where they look: the
	// ones that ignore USLT read the LYRICS user-text frame, which is what the
	// FLAC side has always written and what most taggers emit for MP3 too.
	// Writing only USLT is a file whose lyrics half the players cannot see.
	add("LYRICS", l.Merged)
	add("UNSYNCEDLYRICS", l.Merged)
	add("LYRICS_ORIGINAL", l.Original)
	add("LYRICS_TRANSLATION", l.Translation)
	add("LYRICS_ROMANIZATION", l.Romanization)
	if l.Translation != "" {
		add("LYRICS_TRANSLATION_SOURCE", l.TranslationSource)
		add("LYRICS_TRANSLATION_MODEL", l.TranslationModel)
	}
	if credit.MusicID != 0 {
		add("NETEASE_MUSIC_ID", strconv.FormatInt(credit.MusicID, 10))
	}

	var body []byte
	for _, fr := range kept {
		body = appendFrame(body, fr, version)
	}
	body = append(body, make([]byte, id3v2Padding)...)

	tag := make([]byte, 0, id3v2HeaderSize+len(body))
	tag = append(tag, 'I', 'D', '3', version, 0, 0)
	tag = appendSynchsafe(tag, uint32(len(body)))
	tag = append(tag, body...)
	if _, err := dst.Write(tag); err != nil {
		return err
	}

	// Skip the source's own tag; its frames are already accounted for above.
	if _, err := src.Seek(int64(tagSize), io.SeekStart); err != nil {
		return err
	}
	return copyAudio(src, dst, total, onProgress)
}

// isLyricKey reports whether a user-text frame belongs to the lyrics this
// package owns. The id keys are deliberately not included: the song id is
// handled on its own, because it is the one of them a rewrite may have to
// invent (see rewriteMP3Lyrics), and the rest of the platform keys are
// somebody else's to keep.
func isLyricKey(desc string) bool {
	for _, k := range lyricKeys {
		if strings.EqualFold(desc, k) {
			return true
		}
	}
	return false
}

// lyricsFromComments reads back the timelines this package writes, so a caller
// can compare them against what it was about to write.
//
// LYRICS wins over UNSYNCEDLYRICS when a file has both, because that is the
// order applyLyrics writes them in and the order other taggers tend to follow.
// A file that carries only LYRICS_ORIGINAL — tagged by something that does not
// use the merged key — is read as its original, which is what would have been
// written there anyway.
func lyricsFromComments(c *vorbisComments) Lyrics {
	var l Lyrics
	l.Merged, _ = c.get("LYRICS")
	if strings.TrimSpace(l.Merged) == "" {
		l.Merged, _ = c.get("UNSYNCEDLYRICS")
	}
	l.Original, _ = c.get("LYRICS_ORIGINAL")
	l.Translation, _ = c.get("LYRICS_TRANSLATION")
	l.Romanization, _ = c.get("LYRICS_ROMANIZATION")
	l.TranslationSource, _ = c.get("LYRICS_TRANSLATION_SOURCE")
	l.TranslationModel, _ = c.get("LYRICS_TRANSLATION_MODEL")
	if strings.TrimSpace(l.Original) == "" {
		l.Original = l.Merged
	}
	return l
}

// AudioInfo is what a caller needs to know about a file before deciding what to
// do with it. Everything here comes from one pass over the metadata prefix.
type AudioInfo struct {
	// Path is the file this was read from, carried so a caller can hand the
	// whole value to a writer instead of passing the name alongside it.
	Path    string
	Format  AudioFormat
	Length  int64
	Title   string
	Artists []string
	Album   string

	// The platform's own identifiers, read back from the keys this package
	// writes. A file that predates them, or that was tagged by something else,
	// simply has zeros here.
	MusicID   int64
	AlbumID   int64
	ArtistIDs []int64
	Bitrate   int

	HasCover bool
	// Lyrics is what the file currently carries, so a caller about to write can
	// tell "already done" from "this is a real change" before it copies a
	// hundred megabytes to find out.
	Lyrics Lyrics
	// Duration is exact for FLAC, which states its total sample count, and zero
	// for MP3, where deriving it means parsing frame headers and guessing
	// whether the stream is CBR. A zero means "unknown" and callers must treat
	// it as no evidence rather than as a mismatch.
	Duration time.Duration
}

// HasLyrics reports whether the file already carries lyrics.
func (i *AudioInfo) HasLyrics() bool { return !i.Lyrics.Empty() }

// Inspect reads a file's identifying tags in a single pass.
func Inspect(path string) (*AudioInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	info := &AudioInfo{Path: path, Length: fi.Size()}

	head := make([]byte, 10)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	head = head[:n]

	switch {
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		info.Format = FormatFLAC
		// The cover is reported by its presence, not by its bytes, so the
		// picture payload is stepped over instead of buffered: this runs once
		// per file on every scan of a library.
		blocks, _, _, err := readFLACBlocks(f, false)
		if err != nil {
			return nil, err
		}
		for _, b := range blocks {
			switch b.Type {
			case blockStreamInfo:
				info.Duration = streamInfoDuration(b.Data)
			case blockVorbisComment:
				c, _, err := parseVorbisComments(b.Data)
				if err != nil {
					continue // a broken block should not hide the rest
				}
				info.Title, _ = c.get("TITLE")
				info.Album, _ = c.get("ALBUM")
				if v, ok := c.get("NETEASE_MUSIC_ID"); ok {
					info.MusicID = parseInt64(v)
				}
				if v, ok := c.get("NETEASE_ALBUM_ID"); ok {
					info.AlbumID = parseInt64(v)
				}
				if v, ok := c.get("NETEASE_ARTIST_IDS"); ok {
					info.ArtistIDs = parseIDs(v)
				}
				if v, ok := c.get("NETEASE_BITRATE"); ok {
					info.Bitrate = int(parseInt64(v))
				}
				for _, v := range c.all("ARTIST") {
					info.Artists = append(info.Artists, splitArtists(v)...)
				}
				info.Lyrics = lyricsFromComments(c)
			case blockPicture:
				info.HasCover = true
			}
		}
		return info, nil

	case len(head) >= 3 && string(head[:3]) == "ID3":
		info.Format = FormatMP3
		for _, fr := range readID3FramesLenient(f, head) {
			switch fr.ID {
			case frameTitle:
				info.Title = decodeTextFrame(fr.Body)
			case frameArtists:
				info.Artists = append(info.Artists, splitArtists(decodeTextFrame(fr.Body))...)
			case frameAlbum:
				info.Album = decodeTextFrame(fr.Body)
			case frameLyrics:
				info.Lyrics.Merged = parseUSLT(fr.Body)
			case framePicture:
				info.HasCover = true
			case frameUserText:
				desc, value, ok := parseTXXX(fr.Body)
				if !ok {
					continue
				}
				switch {
				case strings.EqualFold(desc, "NETEASE_MUSIC_ID"):
					info.MusicID = parseInt64(value)
				case strings.EqualFold(desc, "NETEASE_ALBUM_ID"):
					info.AlbumID = parseInt64(value)
				case strings.EqualFold(desc, "NETEASE_ARTIST_IDS"):
					info.ArtistIDs = parseIDs(value)
				case strings.EqualFold(desc, "NETEASE_BITRATE"):
					info.Bitrate = int(parseInt64(value))
				case strings.EqualFold(desc, "LYRICS_ORIGINAL"):
					info.Lyrics.Original = value
				case strings.EqualFold(desc, "LYRICS_TRANSLATION"):
					info.Lyrics.Translation = value
				case strings.EqualFold(desc, "LYRICS_ROMANIZATION"):
					info.Lyrics.Romanization = value
				case strings.EqualFold(desc, "LYRICS_TRANSLATION_SOURCE"):
					info.Lyrics.TranslationSource = value
				case strings.EqualFold(desc, "LYRICS_TRANSLATION_MODEL"):
					info.Lyrics.TranslationModel = value
				case strings.EqualFold(desc, "LYRICS"), strings.EqualFold(desc, "UNSYNCEDLYRICS"):
					// A tagger that wrote the merged timeline as a TXXX rather
					// than a USLT frame; honour it so the file is not rewritten
					// just to say the same thing in a different frame.
					if info.Lyrics.Merged == "" {
						info.Lyrics.Merged = value
					}
				}
			}
		}
		return info, nil

	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		info.Format = FormatMP3
		return info, nil
	}
	return nil, ErrUnsupportedFormat
}

// streamInfoDuration reads the total sample count out of a STREAMINFO block.
//
// The sample rate takes 20 bits and the total sample count 36, so both straddle
// byte boundaries: rate is bytes 10..12 plus the top nibble of 13, and the
// count is the bottom nibble of 13 followed by bytes 14..17.
func streamInfoDuration(d []byte) time.Duration {
	if len(d) < 18 {
		return 0
	}
	rate := int(d[10])<<12 | int(d[11])<<4 | int(d[12])>>4
	if rate <= 0 {
		return 0
	}
	samples := uint64(d[13]&0x0F)<<32 |
		uint64(d[14])<<24 | uint64(d[15])<<16 | uint64(d[16])<<8 | uint64(d[17])
	if samples == 0 {
		return 0
	}
	return time.Duration(samples) * time.Second / time.Duration(rate)
}
