// Package ncm decodes NetEase Cloud Music .ncm containers.
//
// An .ncm file is a thin envelope around an ordinary MP3/FLAC stream:
//
//	magic "CTENFDAM" │ pad │ key blob │ meta blob │ crc+pad │ cover │ RC4(audio)
//
// The two blobs are AES-128-ECB encrypted and hold the RC4 key and the JSON
// metadata respectively. The audio is encrypted with a deliberately weakened
// RC4 whose keystream repeats every 256 bytes, which is what makes fast
// decryption possible.
package ncm

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Keys hard-coded in the NetEase client. They are constants of the file format
// rather than secrets — every open-source NCM decoder ships the same pair.
const (
	coreKey = "hzHRAmso5kInbaxW"
	metaKey = "#14ljk_!\\]&0U<'("
)

const (
	keyBlobPrefix  = "neteasecloudmusic"
	metaBlobPrefix = "163 key(Don't modify):"
	metaJSONPrefix = "music:"
)

// aesECBDecrypt decrypts one AES-128-ECB block chain and strips PKCS#7 padding.
//
// The padding is load-bearing: the derived RC4 key is the remainder after the
// prefix is removed, and forgetting to unpad leaves stray pad bytes on the end
// of the key, which silently yields a wrong keystream rather than an error.
func aesECBDecrypt(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("aes: ciphertext is not a multiple of the block size")
	}
	out := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}
	return pkcs7Unpad(out)
}

func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("aes: empty plaintext")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > aes.BlockSize || n > len(b) {
		return nil, errors.New("aes: invalid PKCS#7 padding")
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errors.New("aes: corrupt PKCS#7 padding")
		}
	}
	return b[:len(b)-n], nil
}

// deriveRC4Key unwraps the key blob into the RC4 key used for the audio.
func deriveRC4Key(blob []byte) ([]byte, error) {
	if len(blob) < len(keyBlobPrefix)+1 {
		return nil, fmt.Errorf("key blob too small (%d bytes)", len(blob))
	}
	xored := make([]byte, len(blob))
	for i, b := range blob {
		xored[i] = b ^ 0x64
	}
	plain, err := aesECBDecrypt([]byte(coreKey), xored)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	key, ok := bytes.CutPrefix(plain, []byte(keyBlobPrefix))
	if !ok {
		return nil, errors.New("key blob is missing the neteasecloudmusic prefix")
	}
	if len(key) == 0 {
		return nil, errors.New("derived RC4 key is empty")
	}
	return key, nil
}

// decryptMetaBlob unwraps the metadata blob into its JSON payload.
func decryptMetaBlob(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty metadata blob")
	}
	xored := make([]byte, len(blob))
	for i, b := range blob {
		xored[i] = b ^ 0x63
	}
	payload, ok := strings.CutPrefix(string(xored), metaBlobPrefix)
	if !ok {
		return nil, errors.New("metadata blob is missing its prefix")
	}
	raw, err := base64.StdEncoding.DecodeString(base64Prefix(payload))
	if err != nil {
		return nil, fmt.Errorf("metadata base64: %w", err)
	}
	plain, err := aesECBDecrypt([]byte(metaKey), raw)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	jsonBytes, ok := bytes.CutPrefix(plain, []byte(metaJSONPrefix))
	if !ok {
		return nil, errors.New("metadata is missing the music: prefix")
	}
	return jsonBytes, nil
}

// base64Prefix returns the leading run of base64 characters. Blobs written by
// older clients can carry trailing junk after the encoded payload; strict
// decoding would reject the whole blob over bytes we do not need anyway.
func base64Prefix(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/', c == '=':
		default:
			return s[:i]
		}
	}
	return s
}

// ncmRC4 is the weakened RC4 variant NCM uses for audio.
//
// Textbook RC4 carries an accumulating index j through the PRGA, so the
// keystream depends on every byte emitted so far. NCM instead derives j from
// the position alone, which makes the keystream a 256-byte cycle that can be
// precomputed once and applied by table lookup.
type ncmRC4 struct {
	ks [256]byte
	// ks2 is the same cycle written twice, so a wide load starting anywhere in
	// the first copy has eight readable bytes after it and needs no wrap
	// handling on the hot path.
	ks2 [512]byte
}

func newNCMRC4(key []byte) *ncmRC4 {
	var s [256]byte
	for i := range s {
		s[i] = byte(i)
	}
	var j byte
	for i := range s {
		j += s[i] + key[i%len(key)]
		s[i], s[j] = s[j], s[i]
	}

	c := &ncmRC4{}
	for off := range c.ks {
		i := (off + 1) & 0xFF // the PRGA's i simply walks with the offset
		a := s[i]
		b := s[(int(a)+i)&0xFF]
		c.ks[off] = s[(int(a)+int(b))&0xFF]
	}
	copy(c.ks2[:], c.ks[:])
	copy(c.ks2[len(c.ks):], c.ks[:])
	return c
}

// xorAt applies the keystream to buf as if buf began at absolute offset off.
//
// This runs once per byte of every file decoded, so it works eight bytes at a
// time: the keystream is a fixed 256-byte cycle, which means a whole machine
// word of it can be loaded and applied with one XOR instead of eight. The tail
// falls back to single bytes, and the offset only ever needs masking once
// because eight steps around a 256-byte cycle return to where they started.
func (c *ncmRC4) xorAt(buf []byte, off int64) {
	i := int(off & 0xFF)
	for len(buf) >= 8 {
		v := binary.LittleEndian.Uint64(buf)
		k := binary.LittleEndian.Uint64(c.ks2[i:])
		binary.LittleEndian.PutUint64(buf, v^k)
		buf = buf[8:]
		i = (i + 8) & 0xFF
	}
	for j := range buf {
		buf[j] ^= c.ks2[i]
		i = (i + 1) & 0xFF
	}
}
