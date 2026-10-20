package server

import "testing"

func TestReactionEmojiPropertyAndSequences(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"👍", true}, {"‼️", true}, {"ℹ️", true}, {"👩‍💻", true},
		{"🇺🇸", true}, {"🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f", true},
		{"1️⃣", true}, {"©️", true}, {"#️", true},
		{"☝🏻", true}, {"✌🏽", true}, {"🖐🏻", true}, {"🕵🏻", true}, {"⛹🏻‍♂️", true},
		{"👩🏻‍❤️‍💋‍👨🏼", true},
		{"1", false}, {"#", false}, {"©", false}, {"‼", false},
		{"©🏻", false}, {"1🏻", false}, {"#🏻", false}, {"©️🏻", false},
		{"abc", false}, {"👍👍", false}, {"👍‍", false},
	} {
		if got := validReactionEmoji(test.value); got != test.valid {
			t.Errorf("validReactionEmoji(%q) = %v, want %v", test.value, got, test.valid)
		}
	}
}
