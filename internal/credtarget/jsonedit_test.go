package credtarget

import (
	"bytes"
	"encoding/json"
	"testing"
)

func mustParse(t *testing.T, doc string) *object {
	t.Helper()
	o, err := parseObject([]byte(doc))
	if err != nil {
		t.Fatalf("parse %q: %v", doc, err)
	}
	return o
}

// The editor's whole job: whatever it did not change comes back exactly as it
// was written, however oddly it was written.
func TestObjectRoundTripsByteForByte(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		`{}`,
		"{ }",
		"\n{\n}\n",
		`{"a":1}`,
		"{\n  \"a\": 1,\n  \"b\": [1, 2,\n 3],\n\t\"c\" :\t{\"d\" : null}\n}\n",
		`{"é\u00e9\"q":"v\\\"","n":-1.5E+10,"t":true,"f":false,"z":null,"o":{"x":{"y":[{}]}}}`,
		"  {\r\n  \"a\"  :  \"x\"  ,\r\n  \"b\":2 }  \r\n",
	} {
		if got := mustParse(t, doc).bytes(); string(got) != doc {
			t.Errorf("round trip changed the document:\n got %q\nwant %q", got, doc)
		}
	}
}

func TestObjectRejectsWhatIsNotAnObject(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{``, `[]`, `"x"`, `1`, `{`, `{"a":}`, `{"a":1,}`, `{"a":1} x`, "\xef\xbb\xbf{}"} {
		if _, err := parseObject([]byte(doc)); err == nil {
			t.Errorf("parseObject(%q) succeeded", doc)
		}
	}
}

func TestObjectSetReplacesInPlaceAndKeepsEverythingElse(t *testing.T) {
	t.Parallel()
	doc := "{\n  \"a\": 1,\n  \"b\":   [1,2],\n  \"c\": \"x\"\n}\n"
	o := mustParse(t, doc)
	o.set("b", []byte(`"new"`))
	want := "{\n  \"a\": 1,\n  \"b\":   \"new\",\n  \"c\": \"x\"\n}\n"
	if got := string(o.bytes()); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestObjectAppendsInTheStyleOfTheDocument(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, doc, want string }{
		{"multi-line", "{\n  \"a\": 1,\n  \"b\": 2\n}", "{\n  \"a\": 1,\n  \"b\": 2,\n  \"n\": 9\n}"},
		{"one member", "{\n    \"a\": 1\n}", "{\n    \"a\": 1,\n    \"n\": 9\n}"},
		{"compact", `{"a":1,"b":2}`, `{"a":1,"b":2,"n":9}`},
		{"compact one", `{"a":1}`, `{"a":1,"n":9}`},
		{"spaced colon", `{"a" : 1, "b" : 2}`, `{"a" : 1, "b" : 2, "n" : 9}`},
		{"empty compact", `{}`, `{"n":9}`},
		{"empty multi-line", "{\n}", "{\n  \"n\": 9\n}"},
		{"empty nested indent", "{\n    }", "{\n      \"n\": 9\n    }"},
		{"crlf", "{\r\n  \"a\": 1\r\n}", "{\r\n  \"a\": 1,\r\n  \"n\": 9\r\n}"},
	}
	for _, tc := range cases {
		o := mustParse(t, tc.doc)
		o.set("n", []byte("9"))
		got := string(o.bytes())
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("%s: result is not JSON: %q", tc.name, got)
		}
	}
}

func TestObjectDeleteKeepsTheRestAndTheCommas(t *testing.T) {
	t.Parallel()
	doc := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}\n"
	for _, tc := range []struct{ drop, want string }{
		{"a", "{\n  \"b\": 2,\n  \"c\": 3\n}\n"},
		{"b", "{\n  \"a\": 1,\n  \"c\": 3\n}\n"},
		{"c", "{\n  \"a\": 1,\n  \"b\": 2\n}\n"},
		{"zz", doc},
	} {
		o := mustParse(t, doc)
		o.delete(tc.drop)
		if got := string(o.bytes()); got != tc.want {
			t.Errorf("delete %s:\n got %q\nwant %q", tc.drop, got, tc.want)
		}
	}
	only := mustParse(t, `{"a":1}`)
	only.delete("a")
	if got := string(only.bytes()); got != `{}` {
		t.Errorf("delete only member: %q", got)
	}
	// Compact documents too.
	compact := mustParse(t, `{"a":1,"b":2,"c":3}`)
	compact.delete("a")
	compact.delete("c")
	if got := string(compact.bytes()); got != `{"b":2}` {
		t.Errorf("compact delete: %q", got)
	}
}

func TestObjectSetCollapsesDuplicateKeys(t *testing.T) {
	t.Parallel()
	o := mustParse(t, `{"a":1,"b":2,"a":3}`)
	o.set("a", []byte("9"))
	if got := string(o.bytes()); got != `{"a":9,"b":2}` {
		t.Fatalf("got %q", got)
	}
}

func TestObjectKeyNamesAreDecoded(t *testing.T) {
	t.Parallel()
	o := mustParse(t, `{"\u0061b":1}`)
	if raw, ok := o.value("ab"); !ok || !bytes.Equal(raw, []byte("1")) {
		t.Fatalf("escaped key not found: %q %v", raw, ok)
	}
}
