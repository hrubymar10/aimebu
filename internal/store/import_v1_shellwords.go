package store

import (
	"errors"
	"strings"
)

// splitShellWords splits a v1 fleet command into words the way v1 runs it: v1 hands the command to `sh -c`, so
// quotes group words and are removed. It supports what a fleet editor writes: plain words, "double quoted"
// words (a backslash escapes ", \, $ and `), 'single quoted' words (literal) and a backslash outside quotes.
// Anything the shell would expand or chain is refused, because a declarative team cannot reproduce it: an
// unquoted or double-quoted $ or backtick, an unquoted ; | & < > ( ) or newline, and an unbalanced quote.
func splitShellWords(command string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == ' ' || r == '\t':
			flush()
		case r == '\'':
			inWord = true
			closed := false
			for i++; i < len(runes); i++ {
				if runes[i] == '\'' {
					closed = true
					break
				}
				word.WriteRune(runes[i])
			}
			if !closed {
				return nil, errors.New("unbalanced quote")
			}
		case r == '"':
			inWord = true
			closed := false
			for i++; i < len(runes); i++ {
				c := runes[i]
				if c == '"' {
					closed = true
					break
				}
				if c == '$' || c == '`' {
					return nil, errors.New("shell expansion")
				}
				if c == '\\' && i+1 < len(runes) && strings.ContainsRune("\"\\$`", runes[i+1]) {
					i++
					word.WriteRune(runes[i])
					continue
				}
				word.WriteRune(c)
			}
			if !closed {
				return nil, errors.New("unbalanced quote")
			}
		case r == '\\':
			if i+1 >= len(runes) {
				return nil, errors.New("trailing backslash")
			}
			i++
			inWord = true
			word.WriteRune(runes[i])
		case strings.ContainsRune(";|&<>()`$\n\r", r):
			return nil, errors.New("shell syntax")
		default:
			inWord = true
			word.WriteRune(r)
		}
	}
	flush()
	return words, nil
}
