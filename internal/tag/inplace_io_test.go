package tag

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// processIO reports how many bytes this process has actually read and written,
// as the kernel counts them.
//
// It is how "the in-place rewrite writes less" is measured rather than assumed:
// a test that only checks the clock would pass on a cache, and a test that only
// checks the result would pass on a full rewrite that happened to end up the
// same size.
func processIO(t *testing.T) (read, write int64) {
	t.Helper()
	f, err := os.Open("/proc/self/io")
	if err != nil {
		t.Skipf("no /proc/self/io on this platform: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		switch strings.TrimSuffix(fields[0], ":") {
		case "read_bytes":
			read = v
		case "write_bytes":
			write = v
		}
	}
	return read, write
}

// The point of the in-place path, stated as a number: changing the cover of a
// large track must not cost the size of the track.
func TestCoverRewriteInPlaceWritesLittle(t *testing.T) {
	// A fixture the size of a real track: 40 MB of "audio", which is what the
	// fallback would have to copy.
	path := filepath.Join(t.TempDir(), "big.flac")
	flacWithPaddingAndAudio(t, path, jpegBlob(40<<10), 256<<10, 40<<20)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	_, w0 := processIO(t)
	if err := SetCover(path, jpegBlob(90<<10), "image/jpeg", nil); err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	_, w1 := processIO(t)

	wrote := w1 - w0
	if wrote > 4<<20 {
		t.Errorf("changing the cover wrote %.1f MB for a %.1f MB file: the audio was copied",
			float64(wrote)/(1<<20), float64(fi.Size())/(1<<20))
	} else {
		t.Logf("wrote %.2f MB for a %.1f MB file (the metadata section is %.1f MB)",
			float64(wrote)/(1<<20), float64(fi.Size())/(1<<20), float64(metaLen(t, path))/(1<<20))
	}

	// And the file is still a whole file.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != fi.Size() {
		t.Errorf("size changed: %d -> %d", fi.Size(), after.Size())
	}
	if art, err := ReadCover(path); err != nil || art == nil || len(art.Data) != 90<<10 {
		t.Errorf("cover after the change: %v, %d bytes", err, len(art.Data))
	}
}

func metaLen(t *testing.T, path string) int {
	t.Helper()
	_, off, err := AudioOffset(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(off)
}
