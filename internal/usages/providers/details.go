package providers

import (
	"fmt"
	"sort"

	"github.com/goccy/go-json"
)

const jsonShapeDetailMaxFields = 50

func fieldDetail(path, typ string) *ErrorDetail {
	return &ErrorDetail{Fields: map[string]string{path: typ}}
}

func detailOrNil(detail *ErrorDetail) *ErrorDetail {
	if detail == nil || len(detail.Fields) == 0 {
		return nil
	}
	return detail
}

// httpStatusDetail augments a body-shape detail with the response status code
// so the manager's transient-error classifier (hasTransientHTTPStatus) can see
// it. Without this the numeric status is lost — jsonShapeDetail only records
// JSON value types — so a transient 429/5xx could not be distinguished from a
// permanent failure and would wrongly blank the last good snapshot. The status
// is recorded as "http_<code>" under the "<prefix>.http_status" key.
func httpStatusDetail(prefix string, data []byte, statusCode int) *ErrorDetail {
	detail := jsonShapeDetail(prefix, data)
	if detail == nil {
		detail = &ErrorDetail{}
	}
	if detail.Fields == nil {
		detail.Fields = map[string]string{}
	}
	detail.Fields[prefix+".http_status"] = fmt.Sprintf("http_%d", statusCode)
	return detail
}

func jsonShapeDetail(prefix string, data []byte) *ErrorDetail {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fieldDetail(prefix, "invalid_json")
	}
	fields := map[string]string{}
	if collectJSONTypes(prefix, value, fields, jsonShapeDetailMaxFields) {
		fields["_truncated"] = "true"
	}
	return &ErrorDetail{Fields: fields}
}

func collectJSONTypes(path string, value any, fields map[string]string, maxFields int) bool {
	if len(fields) >= maxFields {
		return true
	}
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			fields[path] = "object"
			return false
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := v[key]
			if collectJSONTypes(path+"."+key, child, fields, maxFields) {
				return true
			}
		}
		return false
	case []any:
		fields[path] = "array"
	case string:
		fields[path] = "string"
	case float64:
		fields[path] = "number"
	case bool:
		fields[path] = "boolean"
	case nil:
		fields[path] = "null"
	default:
		fields[path] = fmt.Sprintf("%T", value)
	}
	return false
}
