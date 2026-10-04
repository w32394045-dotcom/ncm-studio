package ncm

import "strings"

// Audio formats an NCM container can hold. NetEase serves MP3 and FLAC for
// almost everything; the rest appear in older libraries.
const (
	FormatMP3     = "mp3"
	FormatFLAC    = "flac"
	FormatM4A     = "m4a"
	FormatWAV     = "wav"
	FormatOGG     = "ogg"
	FormatAIFF    = "aiff"
	FormatAPE     = "ape"
	FormatUnknown = ""
)

// DetectFormat identifies the audio from its leading bytes.
//
// The container's own metadata claims a format, but it is not always present
// and has been known to disagree with the payload, so the bytes win.
func DetectFormat(b []byte) string {
	if len(b) < 4 {
		return FormatUnknown
	}
	switch {
	case b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return FormatMP3
	case string(b[:3]) == "ID3":
		return FormatMP3
	case string(b[:4]) == "fLaC":
		return FormatFLAC
	case string(b[:4]) == "RIFF":
		return FormatWAV
	case string(b[:4]) == "OggS":
		return FormatOGG
	case string(b[:4]) == "FORM":
		return FormatAIFF
	case string(b[:4]) == "MAC " || string(b[:4]) == "mac ":
		return FormatAPE
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return FormatM4A
	}

	// Some streams carry a short preamble before the real signature — ID3v2
	// tags on MP3, for instance. Scan a bounded window for a known header.
	limit := min(len(b)-3, 1024)
	for i := range limit {
		switch string(b[i : i+4]) {
		case "fLaC":
			return FormatFLAC
		case "OggS":
			return FormatOGG
		}
		if i < 16 && b[i] == 0xFF && b[i+1]&0xE0 == 0xE0 {
			return FormatMP3
		}
		if i < 32 && string(b[i:i+4]) == "ftyp" {
			return FormatM4A
		}
	}
	return FormatUnknown
}

// Extension returns the file extension for a detected format, including the
// leading dot.
func Extension(format string) string {
	switch format {
	case FormatMP3, FormatFLAC, FormatM4A, FormatWAV, FormatOGG, FormatAIFF, FormatAPE:
		return "." + format
	default:
		return ".audio"
	}
}

// SanitizeName makes a string safe to use as a file name on any platform.
func SanitizeName(name string) string {
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	)
	out := replacer.Replace(name)
	out = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, out)
	out = strings.Trim(out, " .")
	// Keep well clear of the 255-byte limit most filesystems impose, counting
	// bytes rather than runes since CJK titles are three bytes per character.
	for len(out) > 180 {
		out = strings.TrimSpace(out[:180])
		out = strings.TrimRight(out, " .")
	}
	return out
}
