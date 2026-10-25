// Package pricing estimates token cost from a dated, reviewed price list.
package pricing

import (
	_ "embed"

	"github.com/goccy/go-json"
)

type Rate struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	AsOf       string  `json:"as_of"`
	Source     string  `json:"source"`
}

//go:embed models.json
var listJSON []byte

var list = func() map[string]Rate {
	var value struct {
		Models map[string]Rate `json:"models"`
	}
	if err := json.Unmarshal(listJSON, &value); err != nil {
		panic(err)
	}
	return value.Models
}()

// Estimate returns nil for a model without a rate or a turn without both
// input and output counts. The counts must be disjoint: fresh input, cache
// reads, and cache writes are priced separately.
func Estimate(model string, input, output, cacheRead, cacheWrite *int64) *float64 {
	rate, ok := list[model]
	if !ok || input == nil || output == nil || *input < 0 || *output < 0 {
		return nil
	}
	read, write := int64(0), int64(0)
	if cacheRead != nil {
		if *cacheRead < 0 {
			return nil
		}
		read = *cacheRead
	}
	if cacheWrite != nil {
		if *cacheWrite < 0 {
			return nil
		}
		write = *cacheWrite
	}
	cost := (float64(*input)*rate.Input + float64(*output)*rate.Output +
		float64(read)*rate.CacheRead + float64(write)*rate.CacheWrite) / 1_000_000
	return &cost
}
