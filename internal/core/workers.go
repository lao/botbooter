package core

import (
	"context"
	"sync"
)

// workerQueueSize bounds each shard's queue. A full queue makes dispatch block
// until the run context is canceled, which backpressures the adapter's intake
// rather than letting memory grow without bound.
const workerQueueSize = 1024

// workerPool runs the dispatch chain on a fixed set of goroutines, one bounded
// queue per worker. A message is routed to a worker by hashing its ChannelID, so
// a single channel is always handled in order by one worker while different
// channels run concurrently.
type workerPool struct {
	queues []chan *Message
	done   <-chan struct{}
	wg     sync.WaitGroup
}

// startWorkerPool launches n workers bound to ctx, each draining its own queue by
// calling handle. Workers exit when ctx is canceled.
func startWorkerPool(ctx context.Context, n int, handle func(context.Context, *Message)) *workerPool {
	p := &workerPool{
		queues: make([]chan *Message, n),
		done:   ctx.Done(),
	}
	for i := range p.queues {
		q := make(chan *Message, workerQueueSize)
		p.queues[i] = q
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				select {
				case m := <-q:
					handle(ctx, m)
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	return p
}

// submit routes m to its shard's queue. It blocks while that queue is full,
// unblocking if the run context is canceled, in which case m is dropped rather
// than dispatched.
func (p *workerPool) submit(m *Message) {
	i := shardIndex(m.ChannelID, len(p.queues))
	select {
	case p.queues[i] <- m:
	case <-p.done:
	}
}

// stop waits for the workers to return. The run context is canceled by the
// caller before stop runs, so each worker exits once it finishes any message
// already in flight; messages still buffered in a queue are abandoned.
func (p *workerPool) stop() {
	p.wg.Wait()
}

// shardIndex maps a channel id to one of n shards using inline FNV-1a, keeping
// the hot dispatch path allocation-free. n is always >= 1.
func shardIndex(channelID string, n int) int {
	const (
		offset = uint32(2166136261)
		prime  = uint32(16777619)
	)
	h := offset
	for i := 0; i < len(channelID); i++ {
		h ^= uint32(channelID[i])
		h *= prime
	}
	return int(h % uint32(n))
}
