package credtarget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// A credential file belongs to a CLI that rewrites it too (Claude keeps MCP
// OAuth tokens and a gateway pin next to the login, Codex keeps its own keys).
// Re-marshalling the document would reorder keys and reformat every value, so
// the builders edit it instead: the object is split into its members, only the
// members they change are replaced, and every other byte (order, whitespace,
// escapes, number spelling) is written back exactly as it was read.

// member is one `"key": value` pair of an object together with the bytes that
// precede it. head holds everything from the end of the previous member (or the
// opening brace) up to the start of the value: the comma, the key literal and
// the colon, with their whitespace.
type member struct {
	name  string
	head  []byte
	key   []byte // the key literal as written, including its quotes
	value []byte
}

// object is a parsed JSON object that serialises back to its input.
type object struct {
	prefix  []byte // everything up to and including the opening brace
	members []member
	tail    []byte // whitespace, the closing brace and whatever follows it
}

// errNotObject is returned when a document is not a JSON object.
var errNotObject = errors.New("credtarget: not a JSON object")

// parseObject splits data, which must be one valid JSON object, into members.
func parseObject(data []byte) (*object, error) {
	if !json.Valid(data) {
		return nil, errors.New("credtarget: invalid JSON")
	}
	i := skipSpace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, errNotObject
	}
	o := &object{prefix: data[:i+1]}
	i++
	start := i // start of the head of the next member
	for {
		i = skipSpace(data, i)
		if data[i] == '}' {
			o.tail = data[start:]
			return o, nil
		}
		if data[i] == ',' {
			i = skipSpace(data, i+1)
		}
		keyStart := i
		end := scanString(data, i)
		keyRaw := data[keyStart:end]
		var name string
		if err := json.Unmarshal(keyRaw, &name); err != nil {
			return nil, fmt.Errorf("credtarget: bad key: %w", err)
		}
		i = skipSpace(data, end)
		i = skipSpace(data, i+1) // the colon
		valueEnd := scanValue(data, i)
		o.members = append(o.members, member{name: name, head: data[start:i], key: keyRaw, value: data[i:valueEnd]})
		i = valueEnd
		start = i
	}
}

// bytes serialises the object back to JSON.
func (o *object) bytes() []byte {
	var out bytes.Buffer
	out.Write(o.prefix)
	for _, m := range o.members {
		out.Write(m.head)
		out.Write(m.value)
	}
	out.Write(o.tail)
	return out.Bytes()
}

func (o *object) index(name string) int {
	for i, m := range o.members {
		if m.name == name {
			return i
		}
	}
	return -1
}

// value returns the raw value of the first member called name.
func (o *object) value(name string) ([]byte, bool) {
	if i := o.index(name); i >= 0 {
		return o.members[i].value, true
	}
	return nil, false
}

// set replaces the value of name, or appends the member copying the layout of
// the last one. Later members with the same name are removed so the document
// stays unambiguous.
func (o *object) set(name string, value []byte) {
	i := o.index(name)
	if i < 0 {
		o.append(name, value)
		return
	}
	o.members[i].value = value
	o.deleteFrom(name, i+1)
}

func (o *object) append(name string, value []byte) {
	key, _ := json.Marshal(name)
	head := o.newHead(key)
	o.members = append(o.members, member{name: name, head: head, key: key, value: value})
}

// newHead builds the bytes that introduce a member appended at the end, in the
// style of the existing members: same comma and whitespace before the key, same
// spacing around the colon. An empty object is indented one level deeper than
// its closing brace when it spans several lines and is written compact
// otherwise.
func (o *object) newHead(key []byte) []byte {
	if n := len(o.members); n > 0 {
		before, after := splitHead(o.members[n-1])
		if n == 1 {
			// The first member has no comma yet.
			before = append([]byte(","), before...)
		}
		return join(before, key, after)
	}
	if nl := bytes.LastIndexByte(o.tail, '\n'); nl >= 0 {
		closing := o.tail[nl+1:]
		if brace := bytes.IndexByte(closing, '}'); brace >= 0 {
			closing = closing[:brace]
		}
		return join([]byte("\n"), closing, []byte("  "), key, []byte(": "))
	}
	return join(key, []byte(":"))
}

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// splitHead returns the bytes before and after the key literal of m's head.
func splitHead(m member) (before, after []byte) {
	i := bytes.Index(m.head, m.key)
	if i < 0 {
		return nil, []byte(":")
	}
	return m.head[:i], m.head[i+len(m.key):]
}

// delete removes every member called name and reports whether one existed.
func (o *object) delete(name string) bool {
	return o.deleteFrom(name, 0)
}

func (o *object) deleteFrom(name string, from int) bool {
	removed := false
	for i := from; i < len(o.members); {
		if o.members[i].name != name {
			i++
			continue
		}
		removed = true
		o.removeAt(i)
	}
	return removed
}

// removeAt drops member i together with its comma. The comma lives at the start
// of the next member's head, so removing the first member must also strip the
// comma from the new first one.
func (o *object) removeAt(i int) {
	o.members = append(o.members[:i], o.members[i+1:]...)
	if i == 0 && len(o.members) > 0 {
		head := o.members[0].head
		if j := bytes.IndexByte(head, ','); j >= 0 && len(bytes.TrimSpace(head[:j])) == 0 {
			o.members[0].head = append(append([]byte{}, head[:j]...), head[j+1:]...)
		}
	}
}

func skipSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// scanString returns the index just past the string literal starting at i.
func scanString(data []byte, i int) int {
	i++ // opening quote
	for data[i] != '"' {
		if data[i] == '\\' {
			i++
		}
		i++
	}
	return i + 1
}

// scanValue returns the index just past the JSON value starting at i. The input
// was validated, so it only has to find where the value ends.
func scanValue(data []byte, i int) int {
	switch data[i] {
	case '"':
		return scanString(data, i)
	case '{', '[':
		depth := 0
		for {
			switch data[i] {
			case '"':
				i = scanString(data, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
	}
	for i < len(data) {
		switch data[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}
