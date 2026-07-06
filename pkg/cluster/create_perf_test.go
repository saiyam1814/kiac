package cluster

import (
	"fmt"
	"sync/atomic"
	"testing"
)

func TestInParallel(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		fail    map[int]bool // indices whose fn returns an error
		wantErr string       // "" means success
	}{
		{name: "zero calls", n: 0},
		{name: "single success", n: 1},
		{name: "all succeed", n: 8},
		{name: "single failure", n: 4, fail: map[int]bool{2: true}, wantErr: "fn 2 failed"},
		{name: "lowest index wins", n: 6, fail: map[int]bool{5: true, 1: true, 3: true}, wantErr: "fn 1 failed"},
		{name: "first fails", n: 3, fail: map[int]bool{0: true}, wantErr: "fn 0 failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			err := inParallel(c.n, func(i int) error {
				calls.Add(1)
				if c.fail[i] {
					return fmt.Errorf("fn %d failed", i)
				}
				return nil
			})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("inParallel: unexpected error %v", err)
				}
			} else if err == nil || err.Error() != c.wantErr {
				t.Fatalf("inParallel error = %v, want %q", err, c.wantErr)
			}
			// Every fn must run even when siblings fail: execs cannot be
			// cancelled, so the helper never abandons a goroutine.
			if got := int(calls.Load()); got != c.n {
				t.Fatalf("inParallel ran %d of %d fns", got, c.n)
			}
		})
	}
}

func TestInParallelRunsConcurrently(t *testing.T) {
	// Each fn blocks until every other fn has started; the test only
	// completes if all run at the same time rather than sequentially.
	const n = 4
	var started atomic.Int32
	release := make(chan struct{})
	if err := inParallel(n, func(int) error {
		if started.Add(1) == n {
			close(release)
		}
		<-release
		return nil
	}); err != nil {
		t.Fatalf("inParallel: %v", err)
	}
}
