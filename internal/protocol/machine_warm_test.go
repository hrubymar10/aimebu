package protocol

import (
	"testing"

	"github.com/goccy/go-json"
)

// The warm window travels with a delivery: absent is not the same as zero (a server without the setting says nothing; one with it says 0 for off).
func TestDeliveryCarriesTheWarmWindow(t *testing.T) {
	five, off := 300, 0
	for name, in := range map[string]*int{"absent": nil, "off": &off, "five minutes": &five} {
		frame := DeliverFrame(Delivery{AgentID: "agt", TurnID: "turn", WarmIdleSeconds: in})
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		var back Frame
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		got := back.Delivery().WarmIdleSeconds
		if (got == nil) != (in == nil) || (got != nil && *got != *in) {
			t.Errorf("%s: sent %v, read back %v (%s)", name, in, got, raw)
		}
	}
}

// The keep-alive setting travels with a delivery, and a report has its own field name (it must not share the setting's).
func TestDeliveryCarriesTheKeepAliveSettingAndReportsHaveTheirOwnName(t *testing.T) {
	for name, in := range map[string]*KeepAliveSettings{"absent": nil, "off": {}, "claude": {IntervalSeconds: 3000, StopAfterSeconds: 86400}} {
		raw, err := json.Marshal(DeliverFrame(Delivery{AgentID: "agt", TurnID: "turn", KeepAliveSettings: in}))
		if err != nil {
			t.Fatal(err)
		}
		var back Frame
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		got := back.Delivery().KeepAliveSettings
		if (got == nil) != (in == nil) || (got != nil && *got != *in) {
			t.Errorf("%s: sent %v, read back %v", name, in, got)
		}
	}
	raw, _ := json.Marshal(Frame{Type: KeepAliveFrame, KeepAlive: &KeepAliveReport{AgentID: "agt", OK: true, CacheRead: 4253, CacheWrite: 177}})
	var back Frame
	if err := json.Unmarshal(raw, &back); err != nil || back.KeepAlive == nil || back.KeepAlive.CacheRead != 4253 || back.KeepAliveSettings != nil {
		t.Fatalf("report round trip: %v %+v %s", err, back.KeepAlive, raw)
	}
}
