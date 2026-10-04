package ncm

import (
	"bytes"
	"io"
	"testing"
)

// referenceXOR is the obvious byte-at-a-time implementation, kept as the
// definition of what the keystream is supposed to do. xorAt works eight bytes
// at a time for speed, and this is what says the two agree.
func referenceXOR(c *ncmRC4, buf []byte, off int64) {
	start := int(off & 0xFF)
	for i := range buf {
		buf[i] ^= c.ks[(start+i)&0xFF]
	}
}

func TestXorAtMatchesBytewiseReference(t *testing.T) {
	c := newNCMRC4([]byte("a-key-of-some-length"))

	// The lengths straddle the eight-byte lane: everything below it, exactly
	// one lane, a lane and a bit, and a whole cycle plus a remainder — the case
	// where the offset wraps mid-buffer.
	for _, n := range []int{0, 1, 7, 8, 9, 15, 16, 255, 256, 257, 511, 1000} {
		for _, off := range []int64{0, 1, 7, 8, 100, 255, 256, 257, 4095, 1 << 20} {
			src := make([]byte, n)
			for i := range src {
				src[i] = byte(i*7 + 3)
			}

			got := append([]byte(nil), src...)
			want := append([]byte(nil), src...)
			c.xorAt(got, off)
			referenceXOR(c, want, off)

			if !bytes.Equal(got, want) {
				t.Fatalf("xorAt(%d bytes at offset %d) diverged:\n got % x\nwant % x",
					n, off, got, want)
			}
		}
	}
}

// TestXorAtIsItsOwnInverse is the property that matters for a re-read: applying
// the keystream twice must return the original bytes, which is what lets a
// caller decrypt a window, rewind, and decrypt it again identically.
func TestXorAtIsItsOwnInverse(t *testing.T) {
	c := newNCMRC4([]byte("another-key"))
	src := bytes.Repeat([]byte("the quick brown fox"), 20)

	buf := append([]byte(nil), src...)
	c.xorAt(buf, 1234)
	if bytes.Equal(buf, src) {
		t.Fatal("xorAt left the buffer unchanged")
	}
	c.xorAt(buf, 1234)
	if !bytes.Equal(buf, src) {
		t.Error("applying the keystream twice did not restore the input")
	}
}

// TestSkipMatchesReadingAndDiscarding is the guarantee Skip rests on: the
// keystream depends only on position, so seeking over bytes must leave the
// stream in exactly the state reading them would have.
func TestSkipMatchesReadingAndDiscarding(t *testing.T) {
	c := openSample(t)

	// The reference: decrypt everything from zero.
	if err := c.Rewind(); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	full, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}

	// The same stream, reached by skipping several different distances — none
	// of them a multiple of the 256-byte keystream cycle. Only a window of the
	// remainder is compared: a divergence shows up in the first bytes, and
	// decrypting the whole track six more times would make this the slowest
	// test in the suite for no extra assurance.
	const window = 1 << 20
	for _, skip := range []int64{1, 8, 255, 256, 257, 100000} {
		if err := c.Rewind(); err != nil {
			t.Fatalf("Rewind: %v", err)
		}
		if err := c.Skip(skip); err != nil {
			t.Fatalf("Skip(%d): %v", skip, err)
		}
		rest := make([]byte, window)
		if _, err := io.ReadFull(c, rest); err != nil {
			t.Fatalf("read after skip %d: %v", skip, err)
		}
		if !bytes.Equal(rest, full[skip:skip+window]) {
			t.Errorf("skipping %d bytes did not land where reading them would", skip)
		}
	}
}

func TestSkipRefusesToRunPastTheEnd(t *testing.T) {
	c := openSample(t)
	if err := c.Rewind(); err != nil {
		t.Fatal(err)
	}
	if err := c.Skip(c.AudioSize + 1); err == nil {
		t.Error("Skip past the end of the payload was allowed")
	}
	// Having refused, it must not have moved: a caller that ignores the error
	// would otherwise decrypt from an offset nobody asked for.
	if c.pos != 0 {
		t.Errorf("a refused Skip still advanced the position to %d", c.pos)
	}
}

// TestGrowExtendsWithoutLosingBytes checks the window used for FLAC metadata:
// growing it must produce the same bytes as reading that much from scratch.
func TestGrowExtendsWithoutLosingBytes(t *testing.T) {
	c := openSample(t)

	head, err := c.Head(64 << 10)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	for _, size := range []int{64 << 10, 100 << 10, 256 << 10} {
		grown, err := c.Grow(head, size)
		if err != nil {
			t.Fatalf("Grow(%d): %v", size, err)
		}
		want, err := c.Head(size)
		if err != nil {
			t.Fatalf("Head(%d): %v", size, err)
		}
		if !bytes.Equal(grown, want) {
			t.Errorf("Grow(%d) did not match a fresh read of the same window", size)
		}
	}

	// Growing to something already in hand is a no-op rather than a truncation.
	same, err := c.Grow(head, len(head)/2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(same, head) {
		t.Error("Grow to a smaller size did not return the window unchanged")
	}
}

func BenchmarkXorAt(b *testing.B) {
	c := newNCMRC4([]byte("benchmark-key"))
	buf := make([]byte, 512<<10)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.xorAt(buf, int64(i)*int64(len(buf)))
	}
}

func BenchmarkXorAtReference(b *testing.B) {
	c := newNCMRC4([]byte("benchmark-key"))
	buf := make([]byte, 512<<10)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		referenceXOR(c, buf, int64(i)*int64(len(buf)))
	}
}
