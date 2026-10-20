package server

import (
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/goccy/go-json"
)

// The web emoji picker reacts with any glyph from the vendored gemoji database, in every skin tone it offers, so the
// reaction validator has to accept all of them or a pick fails with an error.
func TestValidReactionEmojiAcceptsEveryPickerGlyph(t *testing.T) {
	raw, err := os.ReadFile("../../web/vendor/gemoji/emoji.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Emoji     string `json:"emoji"`
		SkinTones bool   `json:"skin_tones"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) < 1800 {
		t.Fatalf("gemoji database has %d entries", len(entries))
	}
	tones := []string{"", "\U0001F3FB", "\U0001F3FC", "\U0001F3FD", "\U0001F3FE", "\U0001F3FF"}
	for _, entry := range entries {
		for _, tone := range tones {
			glyph := entry.Emoji
			if tone != "" {
				if !entry.SkinTones {
					continue
				}
				// Mirrors withTone in web/js/emoji-data.js: every modifier base except one between two joiners takes the modifier and drops a following variation selector.
				runes := []rune(entry.Emoji)
				var out []rune
				for index := 0; index < len(runes); index++ {
					out = append(out, runes[index])
					shared := index > 0 && index+1 < len(runes) && runes[index-1] == 0x200d && runes[index+1] == 0x200d
					if unicode.Is(reactionEmojiModifierBase, runes[index]) && !shared {
						out = append(out, []rune(tone)...)
						if index+1 < len(runes) && runes[index+1] == 0xfe0f {
							index++
						}
					}
				}
				glyph = string(out)
			}
			if !validReactionEmoji(glyph) {
				t.Errorf("validReactionEmoji(%q) = false (%+q)", glyph, strings.TrimSpace(glyph))
			}
		}
	}
}
