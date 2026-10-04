package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
)

// fingerprintBytes is how much of a file is digested. NCM containers put the
// key, metadata and cover at the very front, so the head is far more
// distinctive than the audio that follows.
const fingerprintBytes = 256 << 10

// Fingerprint identifies a source file cheaply.
//
// Hashing a whole 150 MB track would make scanning a library of them
// painfully slow, and the audio payload adds nothing that the header has not
// already distinguished. Digesting the size together with the first 256 KiB
// survives renames and moves, which a path-keyed cache would not.
func Fingerprint(path string) (string, int64, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", 0, 0, err
	}
	size := fi.Size()

	h := sha256.New()
	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], uint64(size))
	h.Write(sizeBuf[:])

	if _, err := io.Copy(h, io.LimitReader(f, fingerprintBytes)); err != nil {
		return "", 0, 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, fi.ModTime().UnixNano(), nil
}

// FingerprintCached reuses a cached digest when the file is unchanged.
func (s *Store) FingerprintCached(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	size, mtime := fi.Size(), fi.ModTime().UnixNano()
	if hash, ok := s.KnownHash(path, size, mtime); ok {
		return hash, nil
	}
	hash, size, mtime, err := Fingerprint(path)
	if err != nil {
		return "", err
	}
	s.RememberHash(path, hash, size, mtime)
	return hash, nil
}
