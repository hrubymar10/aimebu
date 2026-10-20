package store

import (
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestULIDEncodingAndOrdering(t *testing.T) {
	t.Parallel()
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	const ms = int64(1_700_000_000_123)
	g := new(idGenerator)
	previous := ""
	for i := 0; i < 1000; i++ {
		clock := func() time.Time { return time.UnixMilli(ms) }
		if i == 500 {
			clock = func() time.Time { return time.UnixMilli(ms - 1) }
		}
		id, err := g.next(clock)
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 26 || id[0] > '7' || strings.Trim(id, alphabet) != "" {
			t.Fatalf("invalid Crockford ULID %q", id)
		}
		if previous != "" && id <= previous {
			t.Fatalf("ULID failed monotonic ordering: %q <= %q", id, previous)
		}
		previous = id
		if got := decodedTimestamp(t, id); got != ms {
			t.Fatalf("timestamp = %d, want %d", got, ms)
		}
	}
}

func TestULIDMatchesIndependentBase32(t *testing.T) {
	t.Parallel()
	const ms = int64(1_700_000_000_123)
	g := &idGenerator{lastMS: uint64(ms)}
	for i := range g.random {
		g.random[i] = byte(i*17 + 3)
	}
	wantRaw := make([]byte, 16)
	for i := 0; i < 6; i++ {
		wantRaw[5-i] = byte(uint64(ms) >> (8 * i))
	}
	copy(wantRaw[6:], g.random[:])
	wantRaw[15]++ // next() increments the random part in the same millisecond.

	got, err := g.next(func() time.Time { return time.UnixMilli(ms) })
	if err != nil {
		t.Fatal(err)
	}
	n := new(big.Int).SetBytes(wantRaw)
	want := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.DivMod(n, big.NewInt(32), remainder)
		want[i] = crockford[remainder.Int64()]
		n = quotient
	}
	if got != string(want) {
		t.Fatalf("ULID = %q, independent base32 = %q", got, want)
	}
}

func decodedTimestamp(t *testing.T, id string) int64 {
	t.Helper()
	n := new(big.Int)
	base := big.NewInt(32)
	for _, char := range id {
		digit := strings.IndexRune(crockford, char)
		if digit < 0 {
			t.Fatalf("invalid digit %q", char)
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(digit)))
	}
	n.Rsh(n, 80)
	return n.Int64()
}
