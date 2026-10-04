package store

import (
	"testing"

	"ncm-studio/internal/pipeline"
)

// TestLyricsModesAgreeWithPipeline pins an invariant that is otherwise held up
// by a comment and a bare cast in main.go: the two enums are separate types
// with the same meaning, and every value is converted between them by
// conversion alone. Let them drift and a saved setting silently becomes a
// different mode.
func TestLyricsModesAgreeWithPipeline(t *testing.T) {
	pairs := []struct {
		stored   LyricsMode
		pipeline pipeline.LyricsMode
	}{
		{LyricsOff, pipeline.LyricsOff},
		{LyricsEmbed, pipeline.LyricsEmbed},
		{LyricsEmbedAndFile, pipeline.LyricsEmbedAndFile},
		{LyricsFileOnly, pipeline.LyricsFileOnly},
	}
	for _, p := range pairs {
		if got := pipeline.LyricsMode(p.stored); got != p.pipeline {
			t.Errorf("store mode %d converts to pipeline mode %d, want %d",
				p.stored, got, p.pipeline)
		}
	}
	if len(pairs) != int(LyricsFileOnly)+1 {
		t.Errorf("the table covers %d modes but %d exist", len(pairs), LyricsFileOnly+1)
	}
}

// TestLyricsModeValid covers the guard the settings API applies before storing
// a value that arrived as a plain integer.
func TestLyricsModeValid(t *testing.T) {
	for _, m := range []LyricsMode{LyricsOff, LyricsEmbed, LyricsEmbedAndFile, LyricsFileOnly} {
		if !m.Valid() {
			t.Errorf("mode %d should be valid", m)
		}
	}
	// The values are persisted, so the numbering is part of the format: 0..3
	// have to keep meaning what they meant when earlier versions wrote them.
	if LyricsOff != 0 || LyricsEmbed != 1 || LyricsEmbedAndFile != 2 || LyricsFileOnly != 3 {
		t.Errorf("persisted mode numbering changed: %d %d %d %d",
			LyricsOff, LyricsEmbed, LyricsEmbedAndFile, LyricsFileOnly)
	}
	for _, m := range []LyricsMode{-1, 4, 99} {
		if m.Valid() {
			t.Errorf("mode %d should be rejected", m)
		}
	}
}
