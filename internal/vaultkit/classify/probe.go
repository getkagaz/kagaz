package classify

import (
	"sync"
	"time"
)

// probeTimeout bounds a helper's --probe call.
//
// It is deliberately generous. The first launch of a freshly installed,
// notarised Swift binary pays Gatekeeper verification and a cold dyld start,
// which routinely exceeds a few seconds on a busy machine. A probe budget tight
// enough to trip on that turns one slow start into an entire ingest run
// silently classified by rules, with `classifier: rules` written into every
// sidecar and nothing to explain it.
const probeTimeout = 20 * time.Second

// probeCache memoises a backend's --probe answer.
//
// Only a *decisive* answer is cached: a decoded probe reply, or a helper that
// ran and failed on its own terms. A timeout is never cached, because the
// commonest cause of a slow probe is a one-off cold start, and caching that for
// the process lifetime is exactly the silent whole-run downgrade the generous
// probeTimeout above is trying to avoid. A missing binary is not cached either;
// re-checking costs a stat.
//
// The probe runs while the lock is held, so concurrent callers cause exactly
// one probe rather than a thundering herd.
type probeCache struct {
	mu     sync.Mutex
	cached bool
	ok     bool
	why    string
}

// result returns the cached answer, or runs probe. probe reports availability,
// the reason when unavailable, and whether the answer is decisive enough to
// cache.
func (p *probeCache) result(probe func() (ok bool, why string, decisive bool)) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached {
		return p.ok, p.why
	}
	ok, why, decisive := probe()
	p.ok, p.why = ok, why
	p.cached = decisive
	return ok, why
}
