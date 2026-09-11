package main

import (
	"fmt"
	"sync"
	"time"
)

// byteLimiter is a token bucket whose token is a byte, keyed by client
// address. It bounds egress rather than request rate.
//
// nitrokit.Limiter is the same shape but spends exactly one token per
// call, which cannot express "this response cost 4 MB". Charging by the
// byte matters here because a single map session issues hundreds of small
// Range requests: a requests-per-second ceiling loose enough to let that
// session through does nothing to bound what a mirror pulling the whole
// 7.9 GB archive takes. If a second project needs this, it belongs in
// nitrokit as an AllowN, not copied again.
//
// The bucket may go negative. A request is admitted on a positive
// balance and charged afterwards with what actually went out, so one
// oversized response overdraws the address and the next request waits,
// rather than the response being cut off partway through.
type byteLimiter struct {
	rate  float64 // bytes added per second
	burst float64 // bucket capacity in bytes

	mu        sync.Mutex
	buckets   map[string]*byteBucket
	lastSweep time.Time

	now func() time.Time // injectable clock for tests
}

type byteBucket struct {
	tokens float64
	last   time.Time
}

const (
	sweepInterval = 5 * time.Minute
	minIdleEvict  = 10 * time.Minute
)

// newByteLimiter returns a limiter granting rate bytes per second per key,
// up to burst. It panics on a rate or burst that could never admit a
// request, because that is a startup mistake and not a runtime condition.
func newByteLimiter(rate, burst float64) *byteLimiter {
	if rate <= 0 || burst < 1 {
		panic(fmt.Sprintf("newByteLimiter(%v, %v): rate must be positive and burst at least 1", rate, burst))
	}
	return &byteLimiter{
		rate:    rate,
		burst:   burst,
		buckets: map[string]*byteBucket{},
		now:     time.Now,
	}
}

// allow reports whether key has any budget left. When it does not, it
// returns how long until the balance returns to zero; round it up for a
// Retry-After header.
func (l *byteLimiter) allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refill(key, l.now())
	if b.tokens > 0 {
		return true, 0
	}
	return false, time.Duration(-b.tokens / l.rate * float64(time.Second))
}

// charge subtracts n bytes from key's balance, after the bytes have been
// written.
func (l *byteLimiter) charge(key string, n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill(key, l.now()).tokens -= float64(n)
}

// refill brings key's bucket up to date and returns it, creating a full
// bucket for a key not seen before. Caller holds the mutex.
func (l *byteLimiter) refill(key string, now time.Time) *byteBucket {
	l.sweep(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &byteBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
		return b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	return b
}

// sweep drops buckets that have refilled completely, so a flood of
// distinct addresses cannot grow the map without bound. Eviction hands a
// key a fresh burst on its next request, so a bucket is only dropped once
// keeping it and dropping it are the same thing.
//
// The projection matters. A bucket can be deeply in debt — a client that
// pulled a whole multi-gigabyte archive owes far more than one burst —
// and evicting that key on a fixed idle timeout would let it clear its
// debt by waiting out the timeout instead of waiting out the rate. Debt
// therefore keeps a bucket alive for as long as it takes to pay off, at
// about forty bytes of memory per address. Caller holds the mutex.
func (l *byteLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		idle := now.Sub(b.last)
		if idle >= minIdleEvict && b.tokens+idle.Seconds()*l.rate >= l.burst {
			delete(l.buckets, key)
		}
	}
}
