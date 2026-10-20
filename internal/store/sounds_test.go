package store

import (
	"strings"
	"testing"
)

func TestSoundDisplayName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"ping.mp3":              "ping.mp3",
		"../../etc/My Ping.MP3": "My Ping.MP3",
		`C:\\sounds\\ding.wav`:  "ding.wav",
		"bell\x07\x00.wav":      "bell.wav",
		"":                      "custom.wav",
		"..":                    "custom.wav",
		"   ":                   "custom.wav",
	} {
		if got := SoundDisplayName(in, "wav"); got != want {
			t.Errorf("SoundDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("界", 100) + ".mp3"
	if got := SoundDisplayName(long, "mp3"); len([]rune(got)) != 64 || !strings.HasPrefix(got, "界界") {
		t.Errorf("long name should keep 64 runes: %d", len([]rune(got)))
	}
}
