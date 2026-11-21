// Package ratelimit limits requests per client IP with fixed memory.
//
// Each client gets a token bucket: a burst of limit requests, refilled at
// limit per window. Clients are identified by IPv4 address or IPv6 /64.
// Two coarser tiers stop cheap evasion:
//   - a network tier (IPv4 /24, IPv6 /48) allows 16x limit, so rotating
//     addresses inside one network does not multiply the budget;
//   - for scoped checks, an unscoped client tier allows 2x limit, so moving
//     between scopes (tenants, shops, accounts) does not multiply it either.
//
// Buckets live in fixed-size hash tables (count-min style, 4 rows). Hash
// collisions share a budget, which errs on the side of limiting; an active
// client's debt is never evicted by key churn. Memory does not grow with the
// number of clients. State is per process: with several API instances, each
// enforces its own budget.
package ratelimit

import (
	"hash/maphash"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
	"github.com/xnqOS/go-starter-kit/kit/httpx"
)

// DefaultCapacity is the bucket count per hash row.
const DefaultCapacity = 4096

const rows = 4

type bucket struct {
	tokens      float64
	last        time.Time
	initialized bool
}

func (b *bucket) refill(now time.Time, limit int, window time.Duration) {
	if !b.initialized {
		b.initialized = true
		b.tokens = float64(limit)
		b.last = now
		return
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(float64(limit), b.tokens+float64(elapsed)/float64(window)*float64(limit))
		b.last = now
	}
}

// Limiter is safe for concurrent use.
type Limiter struct {
	mu          sync.Mutex
	clients     [rows][]bucket
	unscoped    [rows][]bucket
	networks    []bucket
	seeds       [rows]maphash.Seed
	networkSeed maphash.Seed
	limit       int
	window      time.Duration
}

// New allows a burst of limit requests per client, refilled over window.
func New(limit int, window time.Duration) *Limiter {
	return NewWithCapacity(DefaultCapacity, limit, window)
}

// NewWithCapacity sets the bucket count per hash row (memory is about
// 4 rows x 2 tables x capacity x 40 bytes; about 1.3 MiB by default). It panics on non-positive values.
func NewWithCapacity(capacity, limit int, window time.Duration) *Limiter {
	if capacity <= 0 || limit <= 0 || window <= 0 {
		panic("ratelimit: capacity, limit and window must be positive")
	}
	l := &Limiter{networks: make([]bucket, max(256, capacity/4)), networkSeed: maphash.MakeSeed(), limit: limit, window: window}
	for row := range l.clients {
		l.clients[row] = make([]bucket, capacity)
		l.unscoped[row] = make([]bucket, capacity)
		l.seeds[row] = maphash.MakeSeed()
	}
	return l
}

// RetryAfter is how long one request's worth of budget takes to refill,
// rounded up to whole seconds.
func (l *Limiter) RetryAfter() time.Duration {
	return time.Duration(max(1, int((l.window/time.Duration(l.limit)+time.Second-1)/time.Second))) * time.Second
}

func addressKeys(ip string) (client, network string) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip, "unknown"
	}
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String(), netip.PrefixFrom(addr, 48).Masked().String()
	}
	return addr.String(), netip.PrefixFrom(addr, 24).Masked().String()
}

// Allow spends one request from ip's budget.
func (l *Limiter) Allow(ip string, now time.Time) bool { return l.AllowScoped("", ip, now) }

// AllowScoped spends one request from ip's budget within scope (for example
// "tenant/shop"). An empty scope is the same as Allow.
func (l *Limiter) AllowScoped(scope, ip string, now time.Time) bool {
	client, network := addressKeys(ip)
	var indices, globalIndices [rows]uint64
	for row := range indices {
		indices[row] = maphash.String(l.seeds[row], scope+"\x00"+client) % uint64(len(l.clients[row]))
		globalIndices[row] = maphash.String(l.seeds[row], client) % uint64(len(l.unscoped[row]))
	}
	ni := maphash.String(l.networkSeed, network) % uint64(len(l.networks))
	l.mu.Lock()
	defer l.mu.Unlock()
	n := &l.networks[ni]
	available := false
	globalAvailable := scope == ""
	for row, index := range indices {
		c := &l.clients[row][index]
		c.refill(now, l.limit, l.window)
		available = available || c.tokens >= 1
		if scope != "" {
			g := &l.unscoped[row][globalIndices[row]]
			g.refill(now, l.limit*2, l.window)
			globalAvailable = globalAvailable || g.tokens >= 1
		}
	}
	n.refill(now, l.limit*16, l.window)
	if !available || !globalAvailable || n.tokens < 1 {
		return false
	}
	for row, index := range indices {
		c := &l.clients[row][index]
		c.tokens = max(0, c.tokens-1)
		if scope != "" {
			g := &l.unscoped[row][globalIndices[row]]
			g.tokens = max(0, g.tokens-1)
		}
	}
	n.tokens--
	return true
}

// Middleware rejects over-budget requests with 429 rate_limited and a
// Retry-After header. scope may be nil; otherwise it names the budget a
// request spends from, such as c.Param("tenant").
//
// The client IP comes from c.ClientIP(), so configure TRUSTED_PROXY_CIDRS
// behind a reverse proxy, or every client shares the proxy's budget.
// Register it before body parsing and database work.
func Middleware(l *Limiter, scope func(*gin.Context) string) gin.HandlerFunc {
	retry := strconv.Itoa(int(l.RetryAfter() / time.Second))
	return func(c *gin.Context) {
		key := ""
		if scope != nil {
			key = scope(c)
		}
		if !l.AllowScoped(key, c.ClientIP(), time.Now()) {
			c.Header("Retry-After", retry)
			httpx.Fail(c, apperror.ErrRateLimited)
			return
		}
		c.Next()
	}
}
