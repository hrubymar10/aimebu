package adaptertest

import (
	"fmt"
	"testing"

	"github.com/goccy/go-json"
)

// AssertStrictOutputSchema checks the object rules required by native strict
// response formats, including objects nested inside arrays and alternatives.
func AssertStrictOutputSchema(t *testing.T, raw []byte) {
	t.Helper()
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	var walk func(any, string)
	walk = func(value any, path string) {
		switch node := value.(type) {
		case map[string]any:
			if isObjectSchema(node) {
				properties, ok := node["properties"].(map[string]any)
				if !ok || node["additionalProperties"] != false {
					t.Errorf("%s: object needs properties and additionalProperties=false", path)
				} else {
					required, ok := node["required"].([]any)
					if !ok || len(required) != len(properties) {
						t.Errorf("%s: required must name all %d properties", path, len(properties))
					} else {
						seen := make(map[string]bool, len(required))
						for _, item := range required {
							key, ok := item.(string)
							if !ok || seen[key] {
								t.Errorf("%s: invalid or duplicate required key %v", path, item)
							}
							seen[key] = true
						}
						for key := range properties {
							if !seen[key] {
								t.Errorf("%s: missing required key %q", path, key)
							}
						}
					}
				}
			}
			for key, child := range node {
				walk(child, path+"."+key)
			}
		case []any:
			for i, child := range node {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(schema, "$")
}

func isObjectSchema(node map[string]any) bool {
	if _, ok := node["properties"]; ok {
		return true
	}
	if node["type"] == "object" {
		return true
	}
	if types, ok := node["type"].([]any); ok {
		for _, kind := range types {
			if kind == "object" {
				return true
			}
		}
	}
	return false
}
