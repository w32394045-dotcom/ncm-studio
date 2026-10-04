package store

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// SizeUnit is the unit a minimum size is expressed in on the settings page.
// It is only how the number is shown and read back; what is stored, compared
// and handed to a scan is always Bytes.
type SizeUnit string

const (
	SizeKB SizeUnit = "kb"
	SizeMB SizeUnit = "mb"
	SizeGB SizeUnit = "gb"
)

// DefaultMinSizeBytes is the minimum a fresh install starts from.
//
// A .ncm that decrypts to less than this is nearly always a truncated download
// or a file that was never really audio, so not offering them for conversion is
// the useful default rather than an opinion imposed on anyone: the setting is
// in the UI, and 0 turns it off.
const DefaultMinSizeBytes int64 = 500 << 10

// MaxMinSizeBytes bounds what the setting may ask for.
//
// It is deliberately far above any real track — a gigabyte is well past the
// largest lossless file this tool has seen — because the only job of the bound
// is to keep a typed number from overflowing the byte arithmetic and silently
// becoming a threshold nothing can pass.
const MaxMinSizeBytes int64 = 1 << 40 // 1 TiB

// MinSize is the smallest .ncm a scan will look at.
//
// The zero value, and any Bytes <= 0, means no filtering at all: a scan lists
// everything it finds. That is what makes "off" a value the setting can
// actually hold rather than a separate flag beside it.
type MinSize struct {
	// Bytes is the threshold itself. Every comparison uses it, so changing
	// which unit the page shows can never move the line.
	Bytes int64 `json:"bytes"`
	// Unit is the unit the settings page last showed the threshold in. It is
	// remembered so that reopening the page shows "500 KB" rather than
	// "0.49 MB": the number is the user's, the unit is how they said it.
	Unit SizeUnit `json:"unit,omitempty"`
}

// DefaultMinSize is the size filter a fresh install starts with.
func DefaultMinSize() MinSize {
	return MinSize{Bytes: DefaultMinSizeBytes, Unit: SizeKB}
}

// Enabled reports whether the filter would leave anything out.
func (m MinSize) Enabled() bool { return m.Bytes > 0 }

// UnitOrKB returns the unit to display, defaulting when none was ever chosen.
func (m MinSize) UnitOrKB() SizeUnit {
	if m.Unit.Valid() {
		return m.Unit
	}
	return SizeKB
}

// Valid reports whether a unit is one this build knows.
func (u SizeUnit) Valid() bool { return u == SizeKB || u == SizeMB || u == SizeGB }

// Factor is how many bytes one of this unit is, or 0 for an unknown unit.
func (u SizeUnit) Factor() int64 {
	switch u {
	case SizeKB:
		return 1 << 10
	case SizeMB:
		return 1 << 20
	case SizeGB:
		return 1 << 30
	}
	return 0
}

// ParseSizeUnit reads a unit as it arrives from the web API or a config file.
func ParseSizeUnit(s string) (SizeUnit, bool) {
	u := SizeUnit(strings.ToLower(strings.TrimSpace(s)))
	if u.Valid() {
		return u, true
	}
	return "", false
}

// Amount is the threshold expressed in the unit, which is what the settings
// page puts in its number field.
func (m MinSize) Amount() float64 {
	if m.Bytes <= 0 {
		return 0
	}
	return float64(m.Bytes) / float64(m.UnitOrKB().Factor())
}

// WithAmount returns the setting a number and a unit describe.
//
// A number that is not positive, or is not a number at all, clears the filter:
// an empty field is the obvious way to say "no minimum", and refusing it would
// leave the user unable to undo the setting. The result is clamped so the byte
// value stays inside what the rest of the program can compare.
func (m MinSize) WithAmount(amount float64, unit SizeUnit) MinSize {
	if !unit.Valid() {
		unit = SizeKB
	}
	out := MinSize{Unit: unit}
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return out
	}
	bytes := amount * float64(unit.Factor())
	if bytes >= float64(MaxMinSizeBytes) {
		out.Bytes = MaxMinSizeBytes
		return out
	}
	if bytes < 1 {
		out.Bytes = 1
		return out
	}
	out.Bytes = int64(bytes)
	return out
}

// Description renders the threshold for a log line or an error message.
func (m MinSize) Description() string {
	if !m.Enabled() {
		return "no minimum size"
	}
	amount := m.Amount()
	text := strconv.FormatFloat(amount, 'f', -1, 64)
	return text + " " + strings.ToUpper(string(m.UnitOrKB()))
}

// ── reading the size filter out of JSON ───────────────────────────────────

// patchKeys returns the keys a JSON object actually carried.
//
// It exists because a saved config and a live API patch need opposite treatment
// of a missing field: a config file written before the size filter existed must
// come up with the default, while a patch that says nothing about it must leave
// the current value alone. Only the raw JSON can tell the two apart.
func patchKeys(raw json.RawMessage) map[string]bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return map[string]bool{}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(fields))
	for k := range fields {
		out[k] = true
	}
	return out
}

// decodeMinSize reads the size filter from one JSON value.
//
// It accepts both shapes a caller might send: the object the settings page
// writes ({"bytes":…,"unit":"kb"}) and a plain size string ("500kb", "1.5gb",
// "0"), which is what a hand-written call or a script would use. A null or an
// empty value is the zero MinSize, which is how the filter is turned off.
func decodeMinSize(raw json.RawMessage) (MinSize, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return MinSize{}, nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return MinSize{}, err
		}
		return ParseMinSize(s), nil
	}
	var m MinSize
	if err := json.Unmarshal(raw, &m); err != nil {
		return MinSize{}, err
	}
	return m.normalise(), nil
}

// normalise clamps a decoded filter into the range the program can use.
//
// NaN and the infinities are refused rather than clamped: they compare false
// against every bound, so a switch alone would pass them straight through, and
// a NaN byte count does not merely show a strange number — encoding/json
// refuses to marshal it at all, which would stop the settings file from being
// written for as long as that value was in it.
func (m MinSize) normalise() MinSize {
	if !m.Unit.Valid() {
		m.Unit = SizeKB
	}
	bytes := float64(m.Bytes)
	switch {
	case math.IsNaN(bytes) || math.IsInf(bytes, 0):
		m.Bytes = DefaultMinSizeBytes
	case m.Bytes <= 0:
		m.Bytes = 0
	case m.Bytes > MaxMinSizeBytes:
		m.Bytes = MaxMinSizeBytes
	}
	return m
}

// ParseMinSize reads a size filter written by hand: "500kb", "1.5 mb", "2GB",
// or "0"/"" for no minimum. A bare number is read as kilobytes, which is the
// unit the setting shows by default. Anything unreadable is no minimum, which
// is the same answer as an empty field.
func ParseMinSize(s string) MinSize {
	text := strings.ToLower(strings.TrimSpace(s))
	if text == "" {
		return MinSize{}
	}
	unit := SizeKB
	switch {
	case strings.HasSuffix(text, "kb"):
		text = strings.TrimSpace(strings.TrimSuffix(text, "kb"))
	case strings.HasSuffix(text, "mb"):
		text, unit = strings.TrimSpace(strings.TrimSuffix(text, "mb")), SizeMB
	case strings.HasSuffix(text, "gb"):
		text, unit = strings.TrimSpace(strings.TrimSuffix(text, "gb")), SizeGB
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return MinSize{}
	}
	return MinSize{}.WithAmount(amount, unit)
}
