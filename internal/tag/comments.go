package tag

import (
	"encoding/binary"
	"errors"
	"strings"
)

// vorbisComments is a Vorbis comment block held as an ordered list.
//
// A list rather than a map, because that is what the block is: an ordered run
// of KEY=value entries. Rewriting one should change the lyrics and nothing
// else, so the order, the spelling of every key and the vendor string are all
// carried through untouched. Unknown keys are the point — the block is where
// other tools keep replay gain, composer, and the encoder line.
type vorbisComments struct {
	vendor  string
	entries []vorbisEntry
}

type vorbisEntry struct{ key, value string }

// errBadComments reports a comment block too malformed to rewrite safely.
var errBadComments = errors.New("tag: Vorbis comment block is malformed")

// parseVorbisComments reads a comment block.
//
// The declared entry count is read and then deliberately ignored. Files written
// by the decryptor this tool descends from stored len(bytes)/8 there instead of
// the number of comments — one real file on this machine declares nine while
// holding three — so the walk is driven by the block length and stops when the
// bytes run out. Trusting the count either loses entries or rejects the block
// outright, and the block length is authoritative either way.
//
// malformed is true when the block was salvageable but did not end cleanly,
// which tells the caller the count it rewrites will differ from the one it read.
func parseVorbisComments(block []byte) (c *vorbisComments, malformed bool, err error) {
	if len(block) < 4 {
		return nil, false, errBadComments
	}
	vendorLen := int(binary.LittleEndian.Uint32(block[:4]))
	if vendorLen < 0 || 4+vendorLen > len(block) {
		return nil, false, errBadComments
	}
	c = &vorbisComments{vendor: string(block[4 : 4+vendorLen])}

	pos := 4 + vendorLen
	if pos+4 > len(block) {
		return c, true, nil // a vendor and no count: legal, if unusual
	}
	declared := int(binary.LittleEndian.Uint32(block[pos : pos+4]))
	pos += 4

	for pos+4 <= len(block) {
		n := int(binary.LittleEndian.Uint32(block[pos : pos+4]))
		pos += 4
		if n < 0 || pos+n > len(block) {
			return c, true, nil // truncated mid-entry: keep what parsed
		}
		entry := string(block[pos : pos+n])
		pos += n
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue // not a KEY=value line; drop it rather than invent one
		}
		c.entries = append(c.entries, vorbisEntry{key: key, value: value})
	}
	return c, len(c.entries) != declared || pos != len(block), nil
}

// get returns the first value for a key, compared case-insensitively as the
// Vorbis comment specification requires.
func (c *vorbisComments) get(key string) (string, bool) {
	for _, e := range c.entries {
		if strings.EqualFold(e.key, key) {
			return e.value, true
		}
	}
	return "", false
}

// all returns every value for a key, in the order the entries appear.
//
// Vorbis comments repeat a key once per value rather than joining, which is how
// this package writes a multi-artist credit — so reading only the first ARTIST
// entry silently drops every artist after the first, and with them the evidence
// a track's identity is matched on.
func (c *vorbisComments) all(key string) []string {
	var out []string
	for _, e := range c.entries {
		if strings.EqualFold(e.key, key) {
			out = append(out, e.value)
		}
	}
	return out
}

// has reports whether the block carries a non-empty value under a key. An
// entry written as "KEY=" counts as absent: a tagger that left the value blank
// was saying it had nothing to put there.
func (c *vorbisComments) has(key string) bool {
	for _, v := range c.all(key) {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// add appends an entry without looking at the ones already using the key.
//
// This is how a repeated field is written: the specification gives ARTIST one
// entry per artist, so filling in a credit must not replace the first one the
// way set does.
func (c *vorbisComments) add(key, value string) {
	if value == "" {
		return
	}
	c.entries = append(c.entries, vorbisEntry{key: key, value: value})
}

// set replaces the first entry with this key, or appends one when there is
// none.
//
// An empty value is ignored rather than written. Callers pass a set of
// timelines of which only some are usually present — most tracks have no
// translation — and storing the absent ones as empty strings would both bloat
// every block and overwrite a translation some other tool had already found.
func (c *vorbisComments) set(key, value string) {
	if value == "" {
		return
	}
	for i := range c.entries {
		if strings.EqualFold(c.entries[i].key, key) {
			c.entries[i] = vorbisEntry{key: key, value: value}
			return
		}
	}
	c.entries = append(c.entries, vorbisEntry{key: key, value: value})
}

// marshal renders the block back to bytes.
//
// The count written here is the real one, so a rewrite silently repairs the
// miscount an older tool may have left behind.
func (c *vorbisComments) marshal() []byte {
	size := 4 + len(c.vendor) + 4
	for _, e := range c.entries {
		size += 4 + len(e.key) + 1 + len(e.value)
	}
	buf := make([]byte, 0, size)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(c.vendor)))
	buf = append(buf, c.vendor...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(c.entries)))
	for _, e := range c.entries {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(e.key)+1+len(e.value)))
		buf = append(buf, e.key...)
		buf = append(buf, '=')
		buf = append(buf, e.value...)
	}
	return buf
}

// splitArtists splits the several spellings of a multi-artist credit that turn
// up in real files: this tool writes "; ", other tools write "/" or NUL.
func splitArtists(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == '/' || r == 0
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
