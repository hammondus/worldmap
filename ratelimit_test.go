package main

import (
	"testing"
	"time"
)

// clock returns a limiter whose time is driven by the returned advance
// function, so the tests never sleep.
func clock(l *byteLimiter) func(time.Duration) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

func TestByteLimiterAdmitsUntilBudgetSpent(t *testing.T) {
	l := newByteLimiter(100, 1000)
	advance := clock(l)

	if ok, _ := l.allow("a"); !ok {
		t.Fatal("first request denied; a new key starts with a full burst")
	}
	l.charge("a", 1000)

	ok, retry := l.allow("a")
	if ok {
		t.Fatal("request admitted after the whole burst was spent")
	}
	if retry != 0 {
		t.Errorf("retryAfter = %v, want 0 at exactly zero balance", retry)
	}

	// Overdraw: the response that spent the burst ran 500 bytes past it.
	l.charge("a", 500)
	ok, retry = l.allow("a")
	if ok {
		t.Fatal("request admitted while the key was in debt")
	}
	if want := 5 * time.Second; retry != want {
		t.Errorf("retryAfter = %v, want %v (500 bytes of debt at 100 B/s)", retry, want)
	}

	advance(5 * time.Second)
	if ok, _ := l.allow("a"); ok {
		t.Error("request admitted at exactly zero balance; allow needs a positive one")
	}
	advance(time.Second)
	if ok, _ := l.allow("a"); !ok {
		t.Error("request denied after the debt was paid off")
	}
}

func TestByteLimiterRefillStopsAtBurst(t *testing.T) {
	l := newByteLimiter(100, 1000)
	advance := clock(l)

	l.charge("a", 400)
	advance(time.Hour)
	l.charge("a", 1000) // a full burst, from a bucket that cannot hold more

	if ok, _ := l.allow("a"); ok {
		t.Error("an hour of refill carried the bucket past its burst")
	}
}

func TestByteLimiterKeysAreIndependent(t *testing.T) {
	l := newByteLimiter(100, 1000)
	clock(l)

	l.charge("a", 5000)
	if ok, _ := l.allow("a"); ok {
		t.Error("the charged key was admitted")
	}
	if ok, _ := l.allow("b"); !ok {
		t.Error("an untouched key was denied because another key was charged")
	}
}

func TestByteLimiterSweepKeepsKeysInDebt(t *testing.T) {
	l := newByteLimiter(100, 1000)
	advance := clock(l)

	l.charge("idle", 100)   // pays off in 1 second
	l.charge("debtor", 1e6) // pays off in about 2.8 hours

	// Long enough to clear the idle key and to pass every fixed eviction
	// threshold, but nowhere near enough to clear the debtor.
	advance(time.Hour)
	l.allow("other") // any call drives the sweep

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.buckets["idle"]; ok {
		t.Error("a fully refilled key was kept; the map grows without bound")
	}
	if _, ok := l.buckets["debtor"]; !ok {
		t.Error("a key still in debt was evicted, which hands it a fresh burst and forgives the debt")
	}
}

func TestNewByteLimiterRejectsUnusableSettings(t *testing.T) {
	for _, tc := range []struct{ rate, burst float64 }{{0, 1000}, {-1, 1000}, {100, 0}} {
		t.Run("", func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("newByteLimiter(%v, %v) did not panic", tc.rate, tc.burst)
				}
			}()
			newByteLimiter(tc.rate, tc.burst)
		})
	}
}
