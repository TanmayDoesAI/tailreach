package main

// Network impairment for the benchmark proxy: per-direction loss, delay+jitter,
// token-bucket rate limit (with a small queue and tail drop, like a real bottleneck
// buffer), scheduled rate changes and blackouts. UDP impairs datagrams; TCP throttles
// and stalls bytes (a partition that retransmits, like a real one).

import (
	"container/heap"
	"math/rand"
	"net"
	"sync"
	"time"
)

type blackout struct {
	start time.Time
	dur   time.Duration
}

type rateChange struct {
	at  time.Time
	bps int64
}

// pathImpair is one direction of an impaired link.
type pathImpair struct {
	mu     sync.Mutex
	loss   float64       // datagram drop probability 0..1
	delay  time.Duration // fixed one-way delay
	jitter time.Duration // 0..jitter extra, uniform
	bps    int64         // 0 = unlimited
	tokens float64        // token bucket, in bytes
	last   time.Time     // last refill
	queue  int            // bytes waiting (UDP tail drop only)
	maxQ   int            // queue limit in bytes before tail drop
	black  []blackout
	changes []rateChange
	nextChg int

	sched  *scheduler
	drops  int64 // tail drops / blackout drops
}

func newPath(lossPct float64, delay, jitter time.Duration, bps int64, maxQ int, black []blackout, s *scheduler) *pathImpair {
	if maxQ <= 0 {
		maxQ = 128 << 10
	}
	return &pathImpair{
		loss: lossPct / 100, delay: delay, jitter: jitter, bps: bps,
		tokens: float64(maxQ), last: time.Now(), maxQ: maxQ, black: black,
		sched: s,
	}
}

// apply decides what happens to a datagram of n bytes at time now.
// It returns the send delay (≥0) or -1 for drop.
func (p *pathImpair) apply(n int, now time.Time) time.Duration {
	d, _ := p.admit(n, now)
	return d
}

// admit is apply, also saying whether the datagram now counts against the queue:
// only those must be released with sent() when they leave.
func (p *pathImpair) admit(n int, now time.Time) (d time.Duration, queued bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.nextChg < len(p.changes) && !now.Before(p.changes[p.nextChg].at) {
		p.bps = p.changes[p.nextChg].bps
		p.nextChg++
	}
	for _, b := range p.black {
		if now.After(b.start) && now.Before(b.start.Add(b.dur)) {
			p.drops++
			return -1, false
		}
	}
	if p.loss > 0 && rand.Float64() < p.loss {
		return -1, false
	}
	d = p.delay
	if p.jitter > 0 {
		d += time.Duration(rand.Int63n(int64(p.jitter) + 1))
	}
	if p.bps > 0 {
		p.tokens += now.Sub(p.last).Seconds() * float64(p.bps) / 8
		if max := float64(p.maxQ); p.tokens > max {
			p.tokens = max
		}
		p.last = now
		if need := float64(n); p.tokens >= need {
			p.tokens -= need // has credit: goes now, and never counts against the queue
		} else {
			wait := (need - p.tokens) * 8 / float64(p.bps)
			if p.queue+n > p.maxQ {
				p.drops++ // buffer full: tail drop, like a bottleneck
				return -1, false // and a dropped packet uses no link time
			}
			p.tokens -= need // negative: later packets wait longer, so order holds
			p.queue += n
			queued = true
			d += time.Duration(wait * float64(time.Second))
		}
	}
	return d, queued
}

func (p *pathImpair) sent(n int) {
	p.mu.Lock()
	p.queue -= n
	p.mu.Unlock()
}

// scheduler runs "send at" tasks in order; one per UDP direction is plenty.
type scheduler struct {
	mu    sync.Mutex
	items taskHeap
	wake  chan struct{}
}

type task struct {
	at time.Time
	fn func()
}

type taskHeap []task

func (h taskHeap) Len() int            { return len(h) }
func (h taskHeap) Less(i, j int) bool  { return h[i].at.Before(h[j].at) }
func (h taskHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *taskHeap) Push(x any)         { *h = append(*h, x.(task)) }
func (h *taskHeap) Pop() any           { old := *h; n := len(old); t := old[n-1]; *h = old[:n-1]; return t }

func newScheduler() *scheduler {
	s := &scheduler{wake: make(chan struct{}, 1)}
	go s.run()
	return s
}

func (s *scheduler) run() {
	for {
		s.mu.Lock()
		var next time.Time
		if len(s.items) > 0 {
			next = s.items[0].at
		}
		s.mu.Unlock()

		var timer *time.Timer
		if next.IsZero() {
			<-s.wake
			continue
		}
		d := time.Until(next)
		if d <= 0 {
			s.pop()
			continue
		}
		timer = time.NewTimer(d)
		select {
		case <-timer.C:
			s.pop()
		case <-s.wake:
			timer.Stop()
		}
	}
}

func (s *scheduler) pop() {
	s.mu.Lock()
	if len(s.items) == 0 {
		s.mu.Unlock()
		return
	}
	t := heap.Pop(&s.items).(task)
	s.mu.Unlock()
	t.fn()
}

func (s *scheduler) add(at time.Time, fn func()) {
	s.mu.Lock()
	heap.Push(&s.items, task{at, fn})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// throttleConn rate-limits and delays a TCP connection's writes (blackout = stall).
type throttleConn struct {
	net.Conn
	p *pathImpair
}

func (c throttleConn) Write(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		chunk := len(b) - n
		if chunk > 16<<10 {
			chunk = 16 << 10
		}
		d, queued := c.p.admit(chunk, time.Now())
		if d < 0 {
			time.Sleep(5 * time.Millisecond) // blackout: stall, let TCP retransmit
		} else {
			time.Sleep(d)
		}
		_, err := c.Conn.Write(b[n : n+chunk])
		if queued {
			c.p.sent(chunk)
		}
		if err != nil {
			return n, err
		}
		n += chunk
	}
	return n, nil
}
