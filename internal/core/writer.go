package core

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/noisyloop/cashino/internal/event"
)

// Persister durably writes a batch of changes in one transaction.
type Persister interface {
	Persist(ctx context.Context, ch *Changes) error
}

// Item is one unit of work for the apply loop: a machine event or a command.
type Item struct {
	Event *event.Event
	Cmd   Cmd
	Done  chan Result // optional; receives the result after commit
}

// Result of an item, delivered only once the batch is durable.
type Result struct {
	Value any
	Err   error
}

// Writer is the single apply-and-persist loop. It drains its queue in
// batches: every item in a batch is applied to the core in order, then the
// batch's changes (events plus derived rows) are persisted in a single
// transaction, then results and acks are delivered.
type Writer struct {
	core     *Core
	p        Persister
	in       chan *Item
	MaxBatch int
	Linger   time.Duration
	Tick     time.Duration
	// OnCommit is called after every durable batch with the highest applied
	// sequence per machine (for acks) and the persisted changes.
	OnCommit func(acks map[string]uint64, ch *Changes)
	log      *slog.Logger

	Batches     atomic.Int64
	Events      atomic.Int64
	LastBatchMS atomic.Int64
	MaxBatchMS  atomic.Int64
}

// NewWriter creates a writer with a bounded queue (backpressure).
func NewWriter(c *Core, p Persister, log *slog.Logger) *Writer {
	return &Writer{core: c, p: p, in: make(chan *Item, 20000), MaxBatch: 2000, Linger: 5 * time.Millisecond, Tick: 250 * time.Millisecond, log: log}
}

// Submit enqueues an item, blocking while the queue is full.
func (w *Writer) Submit(ctx context.Context, it *Item) error {
	select {
	case w.in <- it:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Do runs a command in the apply loop and waits for its durable result.
func (w *Writer) Do(ctx context.Context, cmd Cmd) (any, error) {
	it := &Item{Cmd: cmd, Done: make(chan Result, 1)}
	if err := w.Submit(ctx, it); err != nil {
		return nil, err
	}
	select {
	case r := <-it.Done:
		return r.Value, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// QueueLen is the current backlog.
func (w *Writer) QueueLen() int { return len(w.in) }

// Run processes the queue until ctx is cancelled.
func (w *Writer) Run(ctx context.Context) {
	tick := time.NewTicker(w.Tick)
	defer tick.Stop()
	batch := make([]*Item, 0, w.MaxBatch)
	for {
		batch = batch[:0]
		select {
		case <-ctx.Done():
			return
		case it := <-w.in:
			batch = append(batch, it)
			deadline := time.NewTimer(w.Linger)
		gather:
			for len(batch) < w.MaxBatch {
				select {
				case it := <-w.in:
					batch = append(batch, it)
				case <-deadline.C:
					break gather
				}
			}
			deadline.Stop()
		case <-tick.C:
		}
		w.process(ctx, batch)
	}
}

func (w *Writer) process(ctx context.Context, batch []*Item) {
	start := time.Now()
	results := make([]Result, len(batch))
	acks := map[string]uint64{}
	c := w.core
	c.mu.Lock()
	for i, it := range batch {
		switch {
		case it.Event != nil:
			if err := c.ApplyMachine(it.Event); err != nil {
				results[i].Err = err
			} else if it.Event.Seq > acks[it.Event.Source] {
				acks[it.Event.Source] = it.Event.Seq
			}
		case it.Cmd != nil:
			results[i].Value, results[i].Err = it.Cmd(c)
		}
	}
	ch := c.Drain()
	c.mu.Unlock()

	if !ch.Empty() || len(ch.Streams) > 0 || len(ch.Daily) > 0 {
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			if err = w.p.Persist(ctx, ch); err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			w.log.Error("persist failed, retrying", "attempt", attempt+1, "err", err)
			time.Sleep(time.Duration(100<<attempt) * time.Millisecond)
		}
		if err != nil {
			// In-memory state is ahead of the database; the only safe
			// recovery is to restart and reload from the durable log.
			w.log.Error("persist failed permanently; exiting so state can be reloaded", "err", err)
			os.Exit(3)
		}
	}
	for i, it := range batch {
		if it.Done != nil {
			it.Done <- results[i]
		}
	}
	if w.OnCommit != nil {
		w.OnCommit(acks, ch)
	}
	if len(batch) > 0 {
		w.Batches.Add(1)
		w.Events.Add(int64(len(ch.Events)))
		ms := time.Since(start).Milliseconds()
		w.LastBatchMS.Store(ms)
		if ms > w.MaxBatchMS.Load() {
			w.MaxBatchMS.Store(ms)
		}
	}
}
