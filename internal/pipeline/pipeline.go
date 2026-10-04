// Package pipeline turns an .ncm file into a tagged, playable audio file.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ncm-studio/internal/lyric"
	"ncm-studio/internal/ncm"
	"ncm-studio/internal/tag"
)

// LyricsMode controls what happens to fetched lyrics.
type LyricsMode int

const (
	// LyricsOff skips lyrics entirely.
	LyricsOff LyricsMode = iota
	// LyricsEmbed writes lyrics into the audio file's own tags.
	LyricsEmbed
	// LyricsEmbedAndFile also drops a matching .lrc next to the audio, which
	// is the form most mobile players look for first.
	LyricsEmbedAndFile
	// LyricsFileOnly writes the .lrc and leaves the audio file untouched. The
	// lyrics still have to be fetched; only the expensive half of the work —
	// rewriting a file that can be a hundred megabytes — is skipped.
	LyricsFileOnly
)

// The three questions are separate because the mode answers them
// independently: fetching is needed by every mode but Off, writing the sidecar
// by the two that mention a file, and rewriting the audio by the two that
// mention embedding.
func (m LyricsMode) WantsLyrics() bool { return m != LyricsOff }
func (m LyricsMode) WantsEmbed() bool  { return m == LyricsEmbed || m == LyricsEmbedAndFile }
func (m LyricsMode) WantsFile() bool   { return m == LyricsEmbedAndFile || m == LyricsFileOnly }

// Options configures a single conversion.
type Options struct {
	OutputDir string
	Lyrics    LyricsMode
	// Client fetches lyrics. When nil, lyrics are skipped regardless of mode.
	Client *lyric.Client
}

// Progress reports how far a conversion has got.
type Progress struct {
	Stage string // "decrypt", "lyrics", "tag", "done"
	Done  int64
	Total int64
}

// Result describes a completed conversion.
type Result struct {
	OutputPath string
	LRCPath    string
	Format     string
	Bytes      int64
	Meta       *ncm.Meta
	Lyrics     *lyric.Lyrics

	// Warnings collects non-fatal problems: a track that decrypted fine but
	// whose lyrics could not be fetched, say.
	Warnings []string
}

// Process decrypts one .ncm file into opts.OutputDir.
func Process(ctx context.Context, srcPath string, opts Options, report func(Progress)) (*Result, error) {
	if opts.OutputDir == "" {
		return nil, errors.New("no output directory configured")
	}
	c, err := ncm.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	res := &Result{Meta: c.Meta}

	head, err := c.Head(headWindowFor(c))
	if err != nil {
		return nil, fmt.Errorf("read audio header: %w", err)
	}
	res.Format = ncm.DetectFormat(head)
	if res.Format == ncm.FormatUnknown {
		return nil, fmt.Errorf("unrecognised audio payload (% x)", head[:min(16, len(head))])
	}

	// Lyrics are fetched before anything is written, so a slow or failing
	// request cannot leave a half-written file behind.
	tags := buildTags(c, res.Format)
	if opts.Lyrics.WantsLyrics() && opts.Client != nil && c.Meta != nil && c.Meta.MusicID > 0 {
		notify(report, Progress{Stage: "lyrics"})
		lyr, err := opts.Client.Fetch(ctx, c.Meta.MusicID)
		switch {
		case err == nil:
			res.Lyrics = lyr
		case errors.Is(err, lyric.ErrNotFound):
			res.Warnings = append(res.Warnings, "no lyrics available for this track")
		default:
			res.Warnings = append(res.Warnings, fmt.Sprintf("lyrics: %v", err))
		}
	}
	// The tags carry lyrics only when they are meant to be embedded; the
	// sidecar-only mode fetches the same lyrics and writes them beside the file
	// instead, so leaving them on the tag struct would embed them anyway.
	if res.Lyrics != nil && opts.Lyrics.WantsEmbed() {
		tags.Lyrics = res.Lyrics.Bilingual()
		tags.LyricsOriginal = res.Lyrics.Original
		tags.LyricsTranslation = res.Lyrics.Translation
		tags.LyricsRomanization = res.Lyrics.Romanization
	}

	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	outPath, err := outputPath(c, srcPath, res.Format, opts)
	if err != nil {
		return nil, err
	}
	res.OutputPath = outPath

	if err := write(ctx, c, head, res, tags, report); err != nil {
		return nil, err
	}

	// A sidecar .lrc is written last so it never exists without its audio.
	if opts.Lyrics.WantsFile() && res.Lyrics != nil && res.Lyrics.Bilingual() != "" {
		lrcPath := strings.TrimSuffix(outPath, filepath.Ext(outPath)) + ".lrc"
		body := res.Lyrics.Bilingual()
		// Players expect the original text in the sidecar and pick translations
		// from their own settings, so only the merged timeline goes here.
		if err := os.WriteFile(lrcPath, []byte(body), 0o644); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("write .lrc: %v", err))
		} else {
			res.LRCPath = lrcPath
		}
	}

	notify(report, Progress{Stage: "done"})
	return res, nil
}

// headWindow is how much decrypted audio is inspected up front. It has to be
// large enough to hold a FLAC file's whole metadata chain, covers included.
const headWindow = 1 << 20

// headWindowFor sizes the first read to fit what the file actually carries.
//
// A FLAC metadata chain is dominated by its cover, and the container header
// already said how big that cover is, so guessing a megabyte and then growing
// the window is wasted work on exactly the files that are already the slowest.
// The slack covers the fixed blocks — STREAMINFO, tags and any padding.
func headWindowFor(c *ncm.Container) int {
	if need := len(c.Cover) + 256<<10; need > headWindow {
		return need
	}
	return headWindow
}

func buildTags(c *ncm.Container, format string) *tag.Tags {
	t := &tag.Tags{
		Cover:     c.Cover,
		CoverMIME: c.CoverMIME,
	}
	if c.Meta != nil {
		t.Title = c.Meta.Title()
		t.Artists = c.Meta.ArtistNames()
		t.Album = c.Meta.Album
		// The identifiers are written into the file so the track can still be
		// matched to its NetEase entry after it is renamed or moved, and so the
		// info page can say where it came from without going online.
		t.MusicID = c.Meta.MusicID
		t.AlbumID = c.Meta.AlbumID
		t.Bitrate = c.Meta.Bitrate
		ids := make([]int64, len(c.Meta.Artists))
		for i, a := range c.Meta.Artists {
			ids[i] = a.ID
		}
		t.ArtistIDs = ids
	}
	return t
}

// outputPath picks the destination file name, preferring the track's own
// metadata over the source file name.
func outputPath(c *ncm.Container, srcPath, format string, opts Options) (string, error) {
	base := strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))
	if name := ncm.SanitizeName(c.Meta.DisplayName()); name != "" {
		base = name
	}
	if base == "" {
		base = "track"
	}
	base = ncm.SanitizeName(base)

	ext := ncm.Extension(format)
	candidate := filepath.Join(opts.OutputDir, base+ext)
	if _, err := os.Stat(candidate); err != nil {
		if os.IsNotExist(err) {
			return candidate, nil
		}
		return "", fmt.Errorf("inspect output path: %w", err)
	}

	candidate = filepath.Join(opts.OutputDir, base+" (1)"+ext)
	for i := 2; ; i++ {
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("inspect output path: %w", err)
		}
		candidate = filepath.Join(opts.OutputDir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

// write streams the decrypted audio into a temporary file and renames it into
// place, so an interrupted run never leaves a partial file looking complete.
func write(ctx context.Context, c *ncm.Container, head []byte, res *Result, tags *tag.Tags, report func(Progress)) error {
	tmp := res.OutputPath + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer func() {
		out.Close()
		os.Remove(tmp) // no-op once the rename has happened
	}()

	switch res.Format {
	case ncm.FormatFLAC:
		if err := writeFLAC(ctx, c, head, out, tags); err != nil {
			return err
		}
	case ncm.FormatMP3:
		if err := writeMP3(ctx, c, head, out, tags); err != nil {
			return err
		}
	default:
		// Nothing is known about how these carry tags, so the audio is copied
		// through untouched rather than risk corrupting it.
		if err := c.Rewind(); err != nil {
			return err
		}
		if tags.Lyrics != "" {
			res.Warnings = append(res.Warnings, "lyrics cannot be embedded in "+res.Format)
		}
	}

	if _, err := copyAudio(ctx, c, out, res, report); err != nil {
		return err
	}

	if err := out.Sync(); err != nil {
		return fmt.Errorf("flush output: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(tmp, res.OutputPath); err != nil {
		return fmt.Errorf("finalise output: %w", err)
	}
	// Report the size of the file on disk, tag section included, rather than
	// just the audio bytes that were streamed.
	if fi, err := os.Stat(res.OutputPath); err == nil {
		res.Bytes = fi.Size()
	}
	return nil
}

// writeFLAC rebuilds the metadata section and leaves the container positioned
// at the first audio frame.
func writeFLAC(ctx context.Context, c *ncm.Container, head []byte, out io.Writer, tags *tag.Tags) error {
	src, more, err := tag.ParseFLACMetadata(head)
	if err != nil {
		return fmt.Errorf("parse FLAC metadata: %w", err)
	}
	// The chain runs past the window. Each retry keeps what has already been
	// read and fetches only the new part, so growing the window costs the extra
	// bytes once rather than decrypting the whole prefix again.
	for size := len(head) * 2; more && size <= 16<<20; size *= 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		head, err = c.Grow(head, size)
		if err != nil {
			return fmt.Errorf("read FLAC metadata: %w", err)
		}
		src, more, err = tag.ParseFLACMetadata(head)
		if err != nil {
			return fmt.Errorf("parse FLAC metadata: %w", err)
		}
	}
	if more {
		return errors.New("FLAC metadata chain exceeds 16 MiB")
	}

	if err := tag.WriteFLACMetadata(out, src, tags); err != nil {
		return err
	}
	// Skip the source's own metadata blocks; the frames after them follow. This
	// is a seek rather than a read: the bytes are known and unwanted, and on a
	// file with a large cover they are megabytes of needless decryption.
	if err := c.Rewind(); err != nil {
		return err
	}
	if err := c.Skip(int64(src.FrameOffset)); err != nil {
		return fmt.Errorf("skip FLAC metadata: %w", err)
	}
	return nil
}

// writeMP3 emits our tag and skips any tag the source already carried.
func writeMP3(ctx context.Context, c *ncm.Container, head []byte, out io.Writer, tags *tag.Tags) error {
	if err := tag.WriteID3v2(out, tags); err != nil {
		return err
	}
	if err := c.Rewind(); err != nil {
		return err
	}
	// The source's tag would otherwise sit behind ours as a second, stale tag.
	if n := int64(tag.ExistingID3v2Size(head)); n > 0 {
		if err := c.Skip(n); err != nil {
			return fmt.Errorf("skip source ID3 tag: %w", err)
		}
	}
	return nil
}

// copyBufferSize is the buffer used to move decrypted audio to disk.
//
// io.Copy's default is 32 KiB, which is a good size for a socket and a poor one
// here: the copy runs in user space because every byte has to be decrypted on
// the way through, and each read and write is a separate syscall — 4800 pairs
// for a 150 MB track at the default, 300 at this size. The kernel cannot
// coalesce them for us, because its own copy paths only apply to data that is
// not being transformed, so the buffer is the only lever there is.
//
// Measured on a 150 MB track, best of three, copy only: 0.47s to internal
// storage and 0.54s to /sdcard at 32 KiB, against 0.28s and 0.30s here. The
// gain is larger on slower storage than this, where the syscalls dominate.
//
// The cost is memory, and it is why the buffers are pooled: at most one per
// worker in flight, so the ceiling is this times the concurrency, and the
// default of three workers holds three quarters of a megabyte.
const copyBufferSize = 512 << 10

// copyBuffers hands out the large buffers, so concurrent workers share a small
// pool of them instead of each holding one for its lifetime.
var copyBuffers = sync.Pool{
	New: func() any {
		b := make([]byte, copyBufferSize)
		return &b
	},
}

// copyAudio streams the rest of the decrypted payload, reporting progress.
func copyAudio(ctx context.Context, c *ncm.Container, out io.Writer, res *Result, report func(Progress)) (int64, error) {
	buf := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(buf)

	pw := &progressWriter{w: out, total: c.AudioSize, report: report}
	if _, err := io.CopyBuffer(pw, &contextReader{ctx: ctx, r: c}, *buf); err != nil {
		return pw.written, fmt.Errorf("decrypt audio: %w", err)
	}
	return pw.written, nil
}

// progressWriter counts bytes and throttles progress callbacks.
type progressWriter struct {
	w       io.Writer
	written int64
	total   int64
	report  func(Progress)
	last    time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	if p.report != nil {
		// Throttled: a callback per 32 KiB chunk would swamp the UI for no
		// visible benefit.
		if now := time.Now(); now.Sub(p.last) >= 100*time.Millisecond {
			p.last = now
			p.report(Progress{Stage: "decrypt", Done: p.written, Total: p.total})
		}
	}
	return n, err
}

// contextReader stops a long copy promptly when the caller cancels.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func notify(report func(Progress), p Progress) {
	if report != nil {
		report(p)
	}
}
