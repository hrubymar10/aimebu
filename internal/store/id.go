package store

import (
	"crypto/rand"
	"errors"
	"sync"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var ids idGenerator

type idGenerator struct {
	mu     sync.Mutex
	lastMS uint64
	random [10]byte
}

// newID returns a lexically sortable, process-monotonic ULID with a type prefix.
func newID(prefix string) (string, error) {
	ulid, err := ids.next(time.Now)
	if err != nil {
		return "", err
	}
	return prefix + "_" + ulid, nil
}

func (g *idGenerator) next(now func() time.Time) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := uint64(now().UnixMilli())
	if ms > (1<<48)-1 {
		return "", errors.New("ULID timestamp exceeds 48 bits")
	}
	if ms > g.lastMS {
		if _, err := rand.Read(g.random[:]); err != nil {
			return "", err
		}
		g.lastMS = ms
	} else {
		ms = g.lastMS
		for i := len(g.random) - 1; i >= 0; i-- {
			g.random[i]++
			if g.random[i] != 0 {
				break
			}
			if i == 0 {
				return "", errors.New("ULID entropy exhausted in one millisecond")
			}
		}
	}

	var raw [16]byte
	for i := 5; i >= 0; i-- {
		raw[i] = byte(ms)
		ms >>= 8
	}
	copy(raw[6:], g.random[:])

	var encoded [26]byte
	for i := range encoded {
		var digit byte
		for bit := 0; bit < 5; bit++ {
			position := i*5 + bit - 2 // Two leading zero bits make 130 bits.
			digit <<= 1
			if position >= 0 {
				digit |= (raw[position/8] >> (7 - position%8)) & 1
			}
		}
		encoded[i] = crockford[digit]
	}
	return string(encoded[:]), nil
}

// NewRoomID makes the ID a room is created with when it must be known before the room exists: a worktree made for an agent that starts in a
// new room is named after that room.
func NewRoomID() (string, error) { return newID("room") }
