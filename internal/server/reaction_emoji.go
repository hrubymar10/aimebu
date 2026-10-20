package server

import (
	"unicode"
	"unicode/utf8"
)

// validReactionEmoji accepts one emoji property character or one joined,
// modified, regional, keycap, or subdivision-flag sequence.
func validReactionEmoji(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	runes := []rune(value)
	if len(runes) >= 2 && len(runes) <= 3 && runes[len(runes)-1] == 0x20e3 &&
		(runes[0] == '#' || runes[0] == '*' || runes[0] >= '0' && runes[0] <= '9') {
		return len(runes) == 2 || runes[1] == 0xfe0f
	}
	if len(runes) >= 3 && runes[0] == 0x1f3f4 && runes[len(runes)-1] == 0xe007f {
		for _, current := range runes[1 : len(runes)-1] {
			if current < 0xe0020 || current > 0xe007e {
				return false
			}
		}
		return true
	}
	bases := 0
	joined := false
	var previous rune
	for index, current := range runes {
		switch {
		case current == 0x200d:
			if bases == 0 || joined {
				return false
			}
			joined = true
		case current == 0xfe0f || current == 0xfe0e:
			if bases == 0 || joined {
				return false
			}
		case skinToneModifier(current):
			if bases == 0 || joined || !unicode.Is(reactionEmojiModifierBase, previous) &&
				!(previous == 0xfe0f && index >= 2 && unicode.Is(reactionEmojiModifierBase, runes[index-2])) {
				return false
			}
		case unicode.Is(reactionEmojiProperty, current):
			if !unicode.Is(reactionEmojiPresentation, current) &&
				(index+1 == len(runes) || runes[index+1] != 0xfe0f &&
					!(skinToneModifier(runes[index+1]) && unicode.Is(reactionEmojiModifierBase, current))) {
				return false
			}
			if bases > 0 && !joined && !(bases == 1 && regionalIndicator(previous) && regionalIndicator(current)) {
				return false
			}
			bases++
			joined = false
		default:
			return false
		}
		previous = current
	}
	return bases > 0 && !joined
}

// Previously stored reactions may not meet current emoji rules, but DELETE
// still needs a bounded path value to avoid unbounded event input.
func validReactionRemoval(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value)
}

func skinToneModifier(value rune) bool { return value >= 0x1f3fb && value <= 0x1f3ff }

func regionalIndicator(value rune) bool { return value >= 0x1f1e6 && value <= 0x1f1ff }
