package ncm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Magic is the eight-byte signature every NCM container starts with.
const Magic = "CTENFDAM"

// Sanity ceilings for the length-prefixed sections. They only need to be large
// enough for real files; their job is to stop a corrupt or hostile header from
// asking us to allocate an arbitrary amount of memory.
const (
	maxKeyBlob  = 1 << 20  // 1 MiB
	maxMetaBlob = 1 << 20  // 1 MiB
	maxCover    = 32 << 20 // 32 MiB
)

// Container is an opened .ncm file. It implements io.Reader over the decrypted
// audio stream, so callers can copy it straight into an output file.
type Container struct {
	Meta      *Meta
	Cover     []byte
	CoverMIME string

	AudioOffset int64
	AudioSize   int64

	f   *os.File
	pos int64 // absolute offset within the audio stream
	rc4 *ncmRC4
}

// Open parses the container header and prepares the audio keystream.
func Open(path string) (*Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	c := &Container{f: f}
	if err := c.parseHeader(); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Container) Close() error {
	if c.f == nil {
		return nil
	}
	return c.f.Close()
}

func (c *Container) parseHeader() error {
	var magic [8]byte
	if _, err := io.ReadFull(c.f, magic[:]); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if string(magic[:]) != Magic {
		return errors.New("not an NCM file (bad magic)")
	}
	// Two bytes the client never populates.
	if _, err := c.f.Seek(2, io.SeekCurrent); err != nil {
		return err
	}

	keyBlob, err := readSection(c.f, maxKeyBlob, "key")
	if err != nil {
		return err
	}
	metaBlob, err := readSection(c.f, maxMetaBlob, "metadata")
	if err != nil {
		return err
	}

	// CRC32 followed by a five-byte gap.
	if _, err := c.f.Seek(9, io.SeekCurrent); err != nil {
		return err
	}

	cover, err := readSection(c.f, maxCover, "cover")
	if err != nil {
		return err
	}
	c.Cover = cover
	c.CoverMIME = sniffImage(cover)

	off, err := c.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	c.AudioOffset = off
	if fi, err := c.f.Stat(); err == nil {
		c.AudioSize = fi.Size() - off
	}
	if c.AudioSize < 0 {
		return errors.New("truncated container: no audio payload")
	}

	key, err := deriveRC4Key(keyBlob)
	if err != nil {
		return err
	}
	c.rc4 = newNCMRC4(key)

	if len(metaBlob) > 0 {
		// Metadata is a nicety: a track whose blob will not decrypt is still
		// worth extracting, just without tags.
		if jsonBytes, err := decryptMetaBlob(metaBlob); err == nil {
			if m, err := parseMeta(jsonBytes); err == nil {
				c.Meta = m
			}
		}
	}
	return nil
}

// readSection reads a little-endian uint32 length followed by that many bytes.
// A zero length yields a nil slice, which is how the cover is omitted.
func readSection(r io.Reader, limit uint32, what string) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, fmt.Errorf("read %s length: %w", what, err)
	}
	if n > limit {
		return nil, fmt.Errorf("%s section claims %d bytes (limit %d)", what, n, limit)
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read %s data: %w", what, err)
	}
	return buf, nil
}

// sniffImage identifies the embedded cover by its signature rather than by the
// extension the metadata claims, which is not always present or correct.
func sniffImage(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// Read returns decrypted audio. The container starts positioned at the first
// audio byte, so io.Copy(out, container) writes the whole track.
func (c *Container) Read(p []byte) (int, error) {
	n, err := c.f.Read(p)
	if n > 0 {
		c.rc4.xorAt(p[:n], c.pos)
		c.pos += int64(n)
	}
	return n, err
}

// Rewind positions the container back at the start of the audio.
func (c *Container) Rewind() error {
	if _, err := c.f.Seek(c.AudioOffset, io.SeekStart); err != nil {
		return err
	}
	c.pos = 0
	return nil
}

// Skip advances over n bytes of audio without reading or decrypting them.
//
// The bytes are dropped rather than decoded, which is safe because the
// keystream is a pure function of position: pos still advances by n, so the
// next byte read is decrypted with the same keystream byte it would have had
// if the skipped bytes had gone through Read. It exists for the metadata
// prefix, which can run to megabytes once a cover is embedded in it and which
// would otherwise be decrypted only to be thrown away.
func (c *Container) Skip(n int64) error {
	if n <= 0 {
		return nil
	}
	if rem := c.AudioSize - c.pos; n > rem {
		return fmt.Errorf("skip %d bytes: only %d of audio remain", n, rem)
	}
	if _, err := c.f.Seek(n, io.SeekCurrent); err != nil {
		return err
	}
	c.pos += n
	return nil
}

// Grow returns a window of size bytes from the start of the audio, reusing the
// have bytes the caller already read and fetching only the rest.
//
// FLAC metadata is length-prefixed but has no total length, so the end of the
// chain can only be found by walking it and growing the window until the walk
// completes. Re-reading the whole window each time would decrypt the same
// leading megabytes over and over; this reads only what is new.
func (c *Container) Grow(have []byte, size int) ([]byte, error) {
	if size <= len(have) {
		return have, nil
	}
	if int64(size) > c.AudioSize {
		size = int(c.AudioSize)
	}
	if err := c.Rewind(); err != nil {
		return nil, err
	}
	if err := c.Skip(int64(len(have))); err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	copy(buf, have)
	if _, err := io.ReadFull(c, buf[len(have):]); err != nil &&
		!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if err := c.Rewind(); err != nil {
		return nil, err
	}
	return buf, nil
}

// Head returns the first n decrypted bytes and rewinds, so callers can inspect
// the audio format before committing to a full pass.
func (c *Container) Head(n int) ([]byte, error) {
	if err := c.Rewind(); err != nil {
		return nil, err
	}
	if int64(n) > c.AudioSize {
		n = int(c.AudioSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if err := c.Rewind(); err != nil {
		return nil, err
	}
	return buf, nil
}
