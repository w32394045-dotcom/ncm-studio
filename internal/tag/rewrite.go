package tag

import (
	"errors"
	"io"
	"os"
)

// ProgressFunc reports how far a rewrite has got. total is the source file's
// size, so a caller can show a meaningful bar for what is a full file copy.
type ProgressFunc func(done, total int64)

// SetCover replaces the front cover of an existing audio file.
//
// Every other tag is carried across verbatim: the metadata prefix is copied
// block by block rather than re-encoded, so lyrics, replay gain and fields
// written by other tools survive untouched. Passing a nil cover removes the
// picture instead.
//
// The audio payload is streamed through, not re-encoded, and the result is
// written beside the original and renamed over it, so an interrupted rewrite
// leaves the original intact.
//
// The rewrite holds the per-file lock for the whole copy. It matters more here
// than anywhere else: a cover change and a lyrics write are the two things a
// user does by hand, from two pages that know nothing about each other, and
// both of them stage through the same "<path>.part".
func SetCover(path string, cover []byte, mime string, onProgress ProgressFunc) error {
	release := lockRewrite(path)
	defer release()

	// Opened read-write, not read-only: when the metadata change fits in the
	// chain's padding, it is written back through this same handle instead of
	// being copied to a new file. Only the metadata is ever written, and only
	// after the new chain has been measured against the old one.
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
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
	// Any failure past this point must not leave a stray .part behind.
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(tmp)
		}
	}()

	switch {
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		if coverInPlace(f, cover, mime) == nil {
			// The chain had room: the audio never moved, and the temporary file
			// was created and is about to be removed without ever being
			// written to. On a hundred-megabyte track that is the difference
			// between rewriting all of it and rewriting a few kilobytes.
			os.Remove(tmp)
			return nil
		}
		err = rewriteFLAC(f, out, cover, mime, total, onProgress)
	case len(head) >= 3 && string(head[:3]) == "ID3":
		err = rewriteMP3(f, out, head, cover, mime, total, onProgress)
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		// A bare MP3 with no tag yet: start one.
		err = rewriteMP3(f, out, nil, cover, mime, total, onProgress)
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

// coverInPlace replaces the PICTURE block without copying the audio, reporting
// errNoRoom when the chain has no room for it.
//
// The whole point is that nothing after the metadata moves, so a cover change
// on a large file costs the size of the artwork rather than the size of the
// track. It is safe by construction rather than by convention: the replacement
// chain is measured against the chain it replaces before a byte is written, and
// it is written with a single pwrite at the metadata offset.
func coverInPlace(f *os.File, cover []byte, mime string) error {
	if len(cover) == 0 {
		return errNoRoom
	}
	// The old picture is not wanted: the new one replaces it, and buffering a
	// megabyte of artwork only to overwrite it is the cost this function exists
	// to avoid in the first place.
	blocks, _, _, err := readFLACBlocks(f, false)
	if err != nil {
		return err
	}
	return fitInPadding(f, blocks, blockPicture, buildPicture(cover, mime))
}

func rewriteFLAC(src *os.File, dst *os.File, cover []byte, mime string, total int64, onProgress ProgressFunc) error {
	// The source's own artwork is not wanted: the caller's cover replaces it a
	// few lines below, and on a file with a megabyte of embedded art that is a
	// megabyte not copied out of the window and then discarded.
	blocks, frameOffset, _, err := readFLACBlocks(src, false)
	if err != nil {
		return err
	}

	if _, err := dst.WriteString("fLaC"); err != nil {
		return err
	}
	// Every block except the pictures: the cover is replaced, the rest is
	// preserved exactly as it was found.
	kept := make([]flacBlock, 0, len(blocks)+1)
	for _, b := range blocks {
		if b.Type == blockPicture {
			continue
		}
		kept = append(kept, b)
	}
	if len(cover) > 0 {
		kept = append(kept, flacBlock{Type: blockPicture, Data: buildPicture(cover, mime)})
	}
	for i, b := range kept {
		if err := writeBlock(dst, b.Type, b.Data, i == len(kept)-1); err != nil {
			return err
		}
	}

	if _, err := src.Seek(int64(frameOffset), io.SeekStart); err != nil {
		return err
	}
	return copyAudio(src, dst, total, onProgress)
}

func rewriteMP3(src *os.File, dst *os.File, head []byte, cover []byte, mime string, total int64, onProgress ProgressFunc) error {
	frames, tagSize, version, err := readID3Frames(src, head)
	if err != nil {
		return err
	}
	version, err = rebuildableVersion(frames, tagSize, version)
	if err != nil {
		return err
	}

	kept := make([]id3Frame, 0, len(frames)+1)
	for _, fr := range frames {
		if fr.ID == framePicture {
			continue
		}
		kept = append(kept, fr)
	}
	if len(cover) > 0 {
		kept = append(kept, id3Frame{ID: framePicture, Body: apicFrame(cover, mime)})
	}

	var body []byte
	for _, fr := range kept {
		body = appendFrame(body, fr, version)
	}
	// Only pad a tag that has content; an empty frame list with padding would
	// declare a tag that holds nothing.
	if len(body) > 0 {
		body = append(body, make([]byte, id3v2Padding)...)
	}

	if len(body) > 0 {
		tag := make([]byte, 0, id3v2HeaderSize+len(body))
		tag = append(tag, 'I', 'D', '3', version, 0, 0)
		tag = appendSynchsafe(tag, uint32(len(body)))
		tag = append(tag, body...)
		if _, err := dst.Write(tag); err != nil {
			return err
		}
	}

	// Skip the source's own tag; its frames are already accounted for above.
	if _, err := src.Seek(int64(tagSize), io.SeekStart); err != nil {
		return err
	}
	return copyAudio(src, dst, total, onProgress)
}

// copyAudio streams the payload through, reporting progress as it goes.
func copyAudio(src, dst *os.File, total int64, onProgress ProgressFunc) error {
	if onProgress == nil {
		_, err := io.Copy(dst, src)
		return err
	}
	buf := make([]byte, 1<<20)
	var done int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			onProgress(done, total)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ErrNoCover reports a file that has no embedded picture to read.
var ErrNoCover = errors.New("tag: file has no embedded cover")
