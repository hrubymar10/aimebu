package server

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

func BenchmarkMessageAppendPersistence(b *testing.B) {
	const existingMessages = 10_000
	base := make([]types.Message, 0, existingMessages)
	for i := 1; i <= existingMessages; i++ {
		base = append(base, types.Message{
			ID:        int64(i),
			RoomID:    "bench",
			From:      "alice@test",
			FromKind:  "ai",
			Body:      "message",
			CreatedAt: "2026-01-01T00:00:00Z",
		})
	}

	b.Run("legacy-json-full-rewrite", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "messages.json")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			msgs := append([]types.Message(nil), base...)
			msgs = append(msgs, types.Message{
				ID:        int64(existingMessages + i + 1),
				RoomID:    "bench",
				From:      "alice@test",
				FromKind:  "ai",
				Body:      fmt.Sprintf("new %d", i),
				CreatedAt: "2026-01-01T00:00:01Z",
			})
			data, err := json.MarshalIndent(msgs, "", "  ")
			if err != nil {
				b.Fatal(err)
			}
			atomicWrite(path, data)
		}
	})

	b.Run("sqlite-single-row-insert", func(b *testing.B) {
		s, err := newStore(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		s.mu.Lock()
		for _, msg := range base {
			if err := s.insertMessageSQLiteLocked(msg); err != nil {
				s.mu.Unlock()
				b.Fatal(err)
			}
		}
		s.mu.Unlock()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			msg := types.Message{
				ID:        int64(existingMessages + i + 1),
				RoomID:    "bench",
				From:      "alice@test",
				FromKind:  "ai",
				Body:      fmt.Sprintf("new %d", i),
				CreatedAt: "2026-01-01T00:00:01Z",
			}
			s.mu.Lock()
			if err := s.insertMessageSQLiteLocked(msg); err != nil {
				s.mu.Unlock()
				b.Fatal(err)
			}
			s.mu.Unlock()
		}
	})
}
