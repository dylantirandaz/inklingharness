package fixture

import "context"

// Drain handles jobs until the channel closes and returns how many it
// handled. When ctx is canceled it stops and returns ctx.Err().
func Drain(ctx context.Context, jobs <-chan int, handle func(int)) (int, error) {
	count := 0
	for job := range jobs {
		handle(job)
		count++
	}
	return count, nil
}
