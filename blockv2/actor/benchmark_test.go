package actor_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"cosmossdk.io/log"
	"github.com/rollkit/rollkit/blockv2/actor"
)

// BenchmarkActorCreation tests actor spawning performance
func BenchmarkActorCreation(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		echoActor := &echoActor{responses: make(map[string]string)}
		_, err := system.Spawn(fmt.Sprintf("echo-%d", i), echoActor)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMessageThroughput tests message sending performance
func BenchmarkMessageThroughput(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	counter := &counterActor{}
	pid, err := system.Spawn("counter", counter)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		err := pid.Tell(&incrementMsg{value: 1})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRequestReplyThroughput tests request-reply performance
func BenchmarkRequestReplyThroughput(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	counter := &counterActor{}
	pid, err := system.Spawn("counter", counter)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := pid.Request(&getCountMsg{}, time.Second)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkConcurrentActors tests performance with multiple actors
func BenchmarkConcurrentActors(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create multiple actors
	const numActors = 100
	actors := make([]*actor.PID, numActors)

	for i := 0; i < numActors; i++ {
		counter := &counterActor{}
		pid, err := system.Spawn(fmt.Sprintf("counter-%d", i), counter)
		if err != nil {
			b.Fatal(err)
		}
		actors[i] = pid
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		actorIdx := i % numActors
		err := actors[actorIdx].Tell(&incrementMsg{value: 1})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMailboxSizes tests performance with different mailbox sizes
func BenchmarkMailboxSizes(b *testing.B) {
	sizes := []int{10, 100, 1000, 10000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("MailboxSize-%d", size), func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			logger := log.NewNopLogger()
			system := actor.NewSystem(ctx, logger, actor.WithMailboxSize(size))

			counter := &counterActor{}
			pid, err := system.Spawn("counter", counter)
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				err := pid.Tell(&incrementMsg{value: 1})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkActorParallelism tests parallel message processing
func BenchmarkActorParallelism(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	const numActors = 10
	actors := make([]*actor.PID, numActors)

	for i := 0; i < numActors; i++ {
		counter := &counterActor{}
		pid, err := system.Spawn(fmt.Sprintf("counter-%d", i), counter)
		if err != nil {
			b.Fatal(err)
		}
		actors[i] = pid
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			actorIdx := i % numActors
			err := actors[actorIdx].Tell(&incrementMsg{value: 1})
			if err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

// BenchmarkCircuitBreaker tests circuit breaker performance
func BenchmarkCircuitBreaker(b *testing.B) {
	cb := actor.NewCircuitBreaker("bench", 1000, time.Minute)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = cb.Call(func() error {
			return nil // Always succeed
		})
	}
}

// BenchmarkRateLimiter tests rate limiter performance
func BenchmarkRateLimiter(b *testing.B) {
	limiter := actor.NewRateLimiter("bench", 10000, 1000000) // High limits for benchmarking

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		limiter.AllowOne()
	}
}

// BenchmarkActorRestart tests actor restart performance
func BenchmarkActorRestart(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	supervisor := &actor.AlwaysRestartSupervisor{Delay: time.Millisecond}
	crasher := &crashingActor{}
	pid, err := system.Spawn("crasher", crasher, actor.WithSupervisor(supervisor))
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		err := pid.Tell("crash")
		if err != nil {
			b.Fatal(err)
		}
		// Wait for restart
		time.Sleep(2 * time.Millisecond)
	}
}

// BenchmarkMemoryUsage tests memory allocation patterns
func BenchmarkMemoryUsage(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	const numActors = 1000
	actors := make([]*actor.PID, numActors)

	// Create many actors
	for i := 0; i < numActors; i++ {
		counter := &counterActor{}
		pid, err := system.Spawn(fmt.Sprintf("counter-%d", i), counter)
		if err != nil {
			b.Fatal(err)
		}
		actors[i] = pid
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		actorIdx := i % numActors
		err := actors[actorIdx].Tell(&incrementMsg{value: 1})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkActorStopStart tests stop/start performance
func BenchmarkActorStopStart(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		counter := &counterActor{}
		pid, err := system.Spawn(fmt.Sprintf("counter-%d", i), counter)
		if err != nil {
			b.Fatal(err)
		}

		pid.Stop()
	}
}

// BenchmarkHighFrequencyMessages tests high-frequency messaging
func BenchmarkHighFrequencyMessages(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger, actor.WithMailboxSize(100000))

	// Use a fast processing actor
	fastActor := &fastProcessorActor{}
	pid, err := system.Spawn("fast", fastActor)
	if err != nil {
		b.Fatal(err)
	}

	var wg sync.WaitGroup
	const numGoroutines = 10

	b.ResetTimer()

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < b.N/numGoroutines; i++ {
				err := pid.Tell(i)
				if err != nil {
					b.Fatal(err)
				}
			}
		}()
	}

	wg.Wait()
}

// Test actors for benchmarking

type fastProcessorActor struct {
	processed int64
}

func (f *fastProcessorActor) Receive(ctx actor.Context, msg any) {
	// Minimal processing for benchmarking
	f.processed++
}