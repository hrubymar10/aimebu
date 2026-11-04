package protocol

import (
	"reflect"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

// The providers of a delivery survive the wire in both directions, and a delivery without any adds no field (an older supervisor sees
// the frame it always saw).
func TestDeliveryProvidersCrossTheWire(t *testing.T) {
	t.Parallel()
	provider := adapter.CustomProvider{ID: "corp-llm", Name: "Corp", NPM: "@ai-sdk/openai-compatible", BaseURL: "https://llm.example.com/v1", KeyEnv: "CORP_KEY",
		ContextTokens: 128000, Models: []adapter.CustomProviderModel{{ID: "fast", Name: "Fast"}}}
	frame := DeliverFrame(Delivery{AgentID: "agt_a", TurnID: "turn_a", Providers: []adapter.CustomProvider{provider}})
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var back Frame
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Delivery().Providers; len(got) != 1 || !reflect.DeepEqual(got[0], provider) {
		t.Fatalf("providers after the wire: %+v", got)
	}
	plain, err := json.Marshal(DeliverFrame(Delivery{AgentID: "agt_a", TurnID: "turn_a"}))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(plain, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["providers"]; ok {
		t.Fatalf("a delivery without providers carries the field: %s", plain)
	}
}
