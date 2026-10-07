package fixture

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDrainAll(t *testing.T) {
	jobs := make(chan int, 3)
	jobs <- 1
	jobs <- 2
	jobs <- 3
	close(jobs)
	total := 0
	count, err := Drain(context.Background(), jobs, func(job int) { total += job })
	if count != 3 || err != nil || total != 6 {
		t.Fatalf("Drain = %d, %v, total %d", count, err, total)
	}
}

type outcome struct {
	count int
	err   error
}

func drainAsync(ctx context.Context, jobs <-chan int, handle func(int)) <-chan outcome {
	done := make(chan outcome, 1)
	go func() {
		count, err := Drain(ctx, jobs, handle)
		done <- outcome{count, err}
	}()
	return done
}

func TestDrainCanceledWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := drainAsync(ctx, make(chan int), func(int) {})
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || result.count != 0 {
			t.Fatalf("Drain = %d, %v; want 0, context.Canceled", result.count, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after cancel")
	}
}

func TestDrainCanceledByHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := make(chan int)
	go func() {
		for job := 0; ; job++ {
			select {
			case jobs <- job:
			case <-time.After(3 * time.Second):
				return
			}
		}
	}()
	done := drainAsync(ctx, jobs, func(job int) {
		if job == 2 {
			cancel()
		}
	})
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || result.count < 3 {
			t.Fatalf("Drain = %d, %v; want at least 3, context.Canceled", result.count, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after cancel")
	}
}
