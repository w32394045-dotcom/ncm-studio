package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"ncm-studio/internal/ncm"
	"ncm-studio/internal/tag"
)

// BenchmarkProcess measures a whole conversion: decrypt a real track, write it
// out with its tags. It is the number the optimisations in this package are
// aimed at, so it is worth being able to re-run against a change.
//
// Output goes to the test's temporary directory, which is on the same storage
// as the source here; pointing it at /sdcard instead measures FUSE, where the
// copy buffer matters a great deal more.
func BenchmarkProcess(b *testing.B) {
	src := "/storage/emulated/0/Download/netease/cloudmusic/Music/Lucenzo Don Omar - Danza Kuduro.ncm"
	if _, err := os.Stat(src); err != nil {
		b.Skipf("sample not available: %v", err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(fi.Size())

	// Lyrics are fetched over the network and cached, so the first run would
	// measure the network rather than the disk. The copy is what this is for.
	out := b.TempDir()
	if err := os.MkdirAll(out, 0o755); err != nil {
		b.Fatal(err)
	}
	b.Logf("writing to %s", out)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		res, err := Process(ctx, src, Options{
			OutputDir: out,
			Lyrics:    LyricsOff,
		}, nil)
		cancel()
		if err != nil {
			b.Fatalf("Process: %v", err)
		}
		if res.Bytes == 0 {
			b.Fatal("no output written")
		}
		b.StopTimer()
		os.Remove(res.OutputPath)
		b.StartTimer()
	}
}

// TestAudioPayloadSurvivesUntouched is the check the streaming tag writer and
// the seek-over-metadata change both rest on.
//
// Rewriting a tag means rebuilding the metadata section, skipping the source's
// own copy of it, and copying the frames after it. Skip advances the container
// by seeking rather than reading, which is only sound because the keystream is
// a pure function of position — and this is what says so, against a real file,
// byte for byte.
func TestAudioPayloadSurvivesUntouched(t *testing.T) {
	src := requireSample(t)
	out := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := Process(ctx, src, Options{OutputDir: out, Lyrics: LyricsOff}, nil)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	// What the source container actually holds, decrypted.
	c, err := ncm.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	head, err := c.Head(16 << 20)
	if err != nil {
		t.Fatal(err)
	}
	srcMeta, more, err := tag.ParseFLACMetadata(head)
	if err != nil || more {
		t.Fatalf("parse source metadata: %v (more=%v)", err, more)
	}
	if err := c.Rewind(); err != nil {
		t.Fatal(err)
	}
	if err := c.Skip(int64(srcMeta.FrameOffset)); err != nil {
		t.Fatal(err)
	}
	wantFrames, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}

	// What the written file holds, past its rebuilt metadata.
	f, err := os.Open(res.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	outHead := make([]byte, 16<<20)
	n, err := io.ReadFull(f, outHead)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read output head: %v", err)
	}
	outMeta, more, err := tag.ParseFLACMetadata(outHead[:n])
	if err != nil || more {
		t.Fatalf("parse output metadata: %v (more=%v)", err, more)
	}
	if _, err := f.Seek(int64(outMeta.FrameOffset), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	gotFrames, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}

	if len(gotFrames) != len(wantFrames) {
		t.Fatalf("audio payload is %d bytes, want %d", len(gotFrames), len(wantFrames))
	}
	if !bytes.Equal(gotFrames, wantFrames) {
		for i := range gotFrames {
			if gotFrames[i] != wantFrames[i] {
				t.Fatalf("audio payload differs at byte %d: got %02x want %02x",
					i, gotFrames[i], wantFrames[i])
			}
		}
	}
}
