package ratelimit

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestCapacityDoesNotBlockNewClients(t *testing.T) {
	l := New(20, time.Minute)
	now := time.Now()
	for i := 0; i < 5000; i++ {
		if !l.Allow(fmt.Sprintf("10.%d.%d.1", i/256, i%256), now) {
			t.Fatalf("new client %d blocked after key churn", i)
		}
	}
	for _, row := range l.clients {
		if len(row) != DefaultCapacity {
			t.Fatal("memory grew with client cardinality")
		}
	}
}

func TestKeyRotationCannotResetDebt(t *testing.T) {
	l := New(20, time.Minute)
	now := time.Now()
	for range 20 {
		if !l.Allow("192.0.2.1", now) {
			t.Fatal("initial burst rejected")
		}
	}
	for i := 0; i < 9000; i++ {
		l.Allow(fmt.Sprintf("10.%d.%d.1", i/256, i%256), now)
	}
	if l.Allow("192.0.2.1", now) {
		t.Fatal("key churn reset an active client's debt")
	}
	if !l.Allow("192.0.2.1", now.Add(time.Minute)) {
		t.Fatal("time-based refill failed")
	}
	// One /48 holds 65536 /64 keys but shares one network budget.
	l = New(20, time.Minute)
	accepted := 0
	for i := 0; i < 65536; i++ {
		if l.Allow(fmt.Sprintf("2001:db8:1:%x::1", i), now) {
			accepted++
		}
	}
	if accepted > 320 {
		t.Fatalf("IPv6 prefix rotation admitted %d requests", accepted)
	}
	// Forced collisions share a budget instead of resetting one.
	l = NewWithCapacity(1, 2, time.Minute)
	if !l.Allow("192.0.2.1", now) || !l.Allow("198.51.100.1", now) || l.Allow("203.0.113.1", now) {
		t.Fatal("collisions increased a budget")
	}
}

func TestIPv6AndMappedIPv4Grouping(t *testing.T) {
	l := NewWithCapacity(10, 2, time.Minute)
	now := time.Now()
	if !l.Allow("2001:db8:1:2::1", now) || !l.Allow("2001:db8:1:2::2", now) || l.Allow("2001:db8:1:2::3", now) {
		t.Fatal("IPv6 /64 does not share a budget")
	}
	if !l.Allow("2001:db8:1:3::1", now) || !l.Allow("2001:db8:1:2::3", now.Add(time.Minute)) {
		t.Fatal("prefix separation or refill failed")
	}
	if !l.Allow("192.0.2.1", now) || !l.Allow("::ffff:192.0.2.1", now) || l.Allow("192.0.2.1", now) {
		t.Fatal("mapped IPv4 escaped the budget")
	}
}

func TestConcurrentRequests(t *testing.T) {
	l := NewWithCapacity(4, 20, time.Minute)
	now := time.Now()
	var successes atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if l.Allow("192.0.2.1", now) {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 20 {
		t.Fatalf("concurrent requests admitted %d", successes.Load())
	}
}

func TestScopesCannotMultiplyBudget(t *testing.T) {
	for _, ip := range []string{"192.0.2.1", "2001:db8:1:2::1"} {
		t.Run(ip, func(t *testing.T) {
			l := New(20, time.Minute)
			now := time.Now()
			accepted := 0
			for i := range 100 {
				if l.AllowScoped(fmt.Sprintf("tenant/shop-%d", i), ip, now) {
					accepted++
				}
			}
			if accepted != 40 {
				t.Fatalf("cross-scope client admitted %d requests, want 40", accepted)
			}
			if !l.AllowScoped("other/shop", "198.51.100.1", now) {
				t.Fatal("another client lost its budget")
			}
			if !l.AllowScoped("tenant/new-shop", ip, now.Add(time.Minute)) {
				t.Fatal("unscoped tier did not refill")
			}
		})
	}
}

func TestBucketRefillIsMonotonicAndCapped(t *testing.T) {
	now := time.Now()
	b := bucket{}
	b.refill(now, 2, time.Minute)
	b.tokens = 0
	later := now.Add(30 * time.Second)
	b.refill(later, 2, time.Minute)
	if b.last != later || b.tokens != 1 {
		t.Fatal("elapsed-time refill failed")
	}
	b.refill(now, 2, time.Minute)
	if b.last != later || b.tokens != 1 {
		t.Fatal("backward time changed the balance")
	}
	b.refill(now.Add(time.Hour), 2, time.Minute)
	if b.tokens != 2 {
		t.Fatal("refill exceeded the burst")
	}
}

func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	limited := New(2, time.Minute)
	router.GET("/shops/:shop", Middleware(limited, func(c *gin.Context) string { return c.Param("shop") }), func(c *gin.Context) { c.Status(204) })
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "192.0.2.1:1234"
		router.ServeHTTP(w, req)
		return w
	}
	for range 2 {
		if w := get("/shops/a"); w.Code != 204 {
			t.Fatalf("within budget: %d", w.Code)
		}
	}
	w := get("/shops/a")
	if w.Code != 429 || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("over budget: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := get("/shops/b"); w.Code != 204 {
		t.Fatalf("other scope: %d", w.Code)
	}
}
