package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkScanWritePattern measures what a directory scan costs the store.
//
// The scan looks at every file it finds and caches a digest for each one it had
// not seen before, so this is the shape that matters: hundreds of cache misses
// in a row. Writing the file on every one of them made the scan quadratic in
// the number of files, and on a phone each write is a create-write-rename cycle
// on FUSE storage.
func BenchmarkScanWritePattern(b *testing.B) {
	for _, files := range []int{100, 500} {
		b.Run(fmt.Sprintf("%d-files/coalesced", files), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				root := b.TempDir()
				st, err := Open(filepath.Join(root, "cfg"), DefaultConfig(root))
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				for f := 0; f < files; f++ {
					st.RememberHash(fmt.Sprintf("/music/%d.ncm", f), fmt.Sprintf("hash-%d", f), 1, 2)
				}
				if err := st.Flush(); err != nil {
					b.Fatal(err)
				}
			}
		})

		// The old shape, kept as the yardstick: one full rewrite per file.
		b.Run(fmt.Sprintf("%d-files/per-file", files), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				root := b.TempDir()
				st, err := Open(filepath.Join(root, "cfg"), DefaultConfig(root))
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				for f := 0; f < files; f++ {
					st.RememberHash(fmt.Sprintf("/music/%d.ncm", f), fmt.Sprintf("hash-%d", f), 1, 2)
					if err := st.Flush(); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// BenchmarkRecordBatch measures the same thing for completed conversions, which
// is the other file the store rewrites once per item.
func BenchmarkRecordBatch(b *testing.B) {
	for _, files := range []int{100, 500} {
		b.Run(fmt.Sprintf("%d-records/coalesced", files), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				root := b.TempDir()
				st, err := Open(filepath.Join(root, "cfg"), DefaultConfig(root))
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				for f := 0; f < files; f++ {
					if err := st.Record(Record{SourceHash: fmt.Sprintf("h%d", f), OutputPath: "/out/x.flac"}); err != nil {
						b.Fatal(err)
					}
				}
				if err := st.Flush(); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("%d-records/per-file", files), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				root := b.TempDir()
				st, err := Open(filepath.Join(root, "cfg"), DefaultConfig(root))
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				for f := 0; f < files; f++ {
					if err := st.Record(Record{SourceHash: fmt.Sprintf("h%d", f), OutputPath: "/out/x.flac"}); err != nil {
						b.Fatal(err)
					}
					if err := st.Flush(); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
