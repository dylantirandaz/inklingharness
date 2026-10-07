package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cache "fixture"
)

type result struct {
	value string
	err error
}

// A Done call tells the test that Get has reached its wait operation.
type observedContext struct {
	context.Context
	ready chan struct{}
	once sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

func observe(ctx context.Context) *observedContext {
	return &observedContext{Context: ctx, ready: make(chan struct{})}
}

func get(c *cache.Cache, ctx context.Context, key string) <-chan result {
	out := make(chan result, 1)
	go func() {
		v, err := c.Get(ctx, key)
		out <- result{v, err}
	}()
	return out
}

func want(t *testing.T, ch <-chan result, value string, err error) {
	t.Helper()
	r := <-ch
	if r.value != value || !errors.Is(r.err, err) {
		t.Fatalf("Get = (%q, %v), want (%q, %v)", r.value, r.err, value, err)
	}
}

func TestCoalescedLoadsAndCachedValue(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := cache.New(func(context.Context, string) (string, error) {
		if calls.Add(1) == 1 { close(started) }
		<-release
		return "value", nil
	})
	first := get(c, context.Background(), "same")
	<-started
	var results []<-chan result
	for i := 0; i < 12; i++ {
		ctx := observe(context.Background())
		results = append(results, get(c, ctx, "same"))
		<-ctx.ready
	}
	close(release)
	want(t, first, "value", nil)
	for _, ch := range results { want(t, ch, "value", nil) }
	want(t, get(c, context.Background(), "same"), "value", nil)
	if calls.Load() != 1 { t.Fatalf("loads = %d", calls.Load()) }
}

func TestFirstCallerCancellationDoesNotCancelLoad(t *testing.T) {
	started, release := make(chan context.Context, 1), make(chan struct{})
	type contextKey struct{}
	base := context.WithValue(context.Background(), contextKey{}, "trace")
	ctx, cancel := context.WithDeadline(base, time.Unix(1<<40, 0))
	defer cancel()
	c := cache.New(func(ctx context.Context, key string) (string, error) {
		started <- ctx
		select {
		case <-ctx.Done(): return "", ctx.Err()
		case <-release: return key, nil
		}
	})
	first := get(c, ctx, "value")
	loadContext := <-started
	follower := observe(context.Background())
	second := get(c, follower, "value")
	<-follower.ready
	cancel()
	want(t, first, "", context.Canceled)
	close(release)
	if _, ok := loadContext.Deadline(); ok { t.Error("load inherited deadline") }
	if loadContext.Err() != nil { t.Errorf("load inherited cancellation: %v", loadContext.Err()) }
	if loadContext.Value(contextKey{}) != "trace" { t.Error("load lost context value") }
	want(t, second, "value", nil)
	want(t, get(c, context.Background(), "value"), "value", nil)
}

func TestFollowerCancellationAndIndependentKey(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := cache.New(func(_ context.Context, key string) (string, error) {
		if key == "slow" { close(started); <-release }
		return key, nil
	})
	first := get(c, context.Background(), "slow")
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	follower := observe(ctx)
	second := get(c, follower, "slow")
	<-follower.ready
	cancel()
	want(t, second, "", context.Canceled)
	want(t, get(c, context.Background(), "fast"), "fast", nil)
	close(release)
	want(t, first, "slow", nil)
}

func TestFailureIsEvictedAndIdentityIsKept(t *testing.T) {
	failure := errors.New("backend unavailable")
	calls := 0
	c := cache.New(func(context.Context, string) (string, error) {
		calls++
		if calls == 1 { return "", failure }
		return "recovered", nil
	})
	want(t, get(c, context.Background(), "k"), "", failure)
	want(t, get(c, context.Background(), "k"), "recovered", nil)
	want(t, get(c, context.Background(), "k"), "recovered", nil)
	if calls != 2 { t.Fatalf("loads = %d", calls) }
}

func TestAlreadyCanceledNeverLoadsOrReturnsCachedValue(t *testing.T) {
	var calls atomic.Int32
	c := cache.New(func(context.Context, string) (string, error) {
		calls.Add(1)
		return "ok", nil
	})
	want(t, get(c, context.Background(), "cached"), "ok", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	want(t, get(c, ctx, "new"), "", context.Canceled)
	want(t, get(c, ctx, "cached"), "", context.Canceled)
	if calls.Load() != 1 { t.Fatalf("canceled call started a load: %d", calls.Load()) }
}

func TestEmptyValueIsCached(t *testing.T) {
	var calls atomic.Int32
	c := cache.New(func(context.Context, string) (string, error) {
		calls.Add(1)
		return "", nil
	})
	want(t, get(c, context.Background(), "empty"), "", nil)
	want(t, get(c, context.Background(), "empty"), "", nil)
	if calls.Load() != 1 { t.Fatalf("empty value loads = %d", calls.Load()) }
}
