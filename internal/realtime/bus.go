package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/namesarnav/synapse/internal/runtime"
)

// Channel is the Redis pub/sub channel carrying event batches.
const Channel = "synapse:events"

// Bus connects the runtime's OnEvents hook to a Hub. With Redis, events cross
// process boundaries (workers and schedulers publish, API instances subscribe);
// without it they are dispatched locally only. Redis holds no state, and losing
// messages is safe because subscribers also tail the durable event log.
type Bus struct {
	Hub   *Hub
	Redis *redis.Client
	Log   *slog.Logger
	// PublishOnly skips the Redis subscription (workers and schedulers).
	PublishOnly bool

	out chan []runtime.Event
}

// NewBus returns a bus; redisClient may be nil.
func NewBus(hub *Hub, redisClient *redis.Client, log *slog.Logger) *Bus {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bus{Hub: hub, Redis: redisClient, Log: log, out: make(chan []runtime.Event, 1024)}
}

// Publish is the runtime.OnEvents hook. It never blocks the caller.
func (b *Bus) Publish(evs []runtime.Event) {
	if b.Redis == nil {
		b.Hub.Dispatch(evs)
		return
	}
	select {
	case b.out <- evs:
	default: // subscribers catch up from the event log
	}
}

// Run pumps outgoing events to Redis and incoming ones to the hub until ctx ends.
func (b *Bus) Run(ctx context.Context) {
	if b.Redis == nil {
		<-ctx.Done()
		return
	}
	if b.PublishOnly {
		b.publishLoop(ctx)
		return
	}
	go b.publishLoop(ctx)
	b.subscribeLoop(ctx)
}

func (b *Bus) publishLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evs := <-b.out:
			raw, err := json.Marshal(evs)
			if err != nil {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if err := b.Redis.Publish(pctx, Channel, raw).Err(); err != nil && ctx.Err() == nil {
				b.Log.Warn("event publish failed", "err", err)
			}
			cancel()
		}
	}
}

func (b *Bus) subscribeLoop(ctx context.Context) {
	backoff := 200 * time.Millisecond
	for ctx.Err() == nil {
		sub := b.Redis.Subscribe(ctx, Channel)
		if _, err := sub.Receive(ctx); err != nil {
			_ = sub.Close()
			if ctx.Err() != nil {
				return
			}
			b.Log.Warn("event subscribe failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 200 * time.Millisecond
		ch := sub.Channel()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case m, ok := <-ch:
				if !ok {
					break loop
				}
				var evs []runtime.Event
				if err := json.Unmarshal([]byte(m.Payload), &evs); err != nil {
					b.Log.Warn("bad event payload", "err", err)
					continue
				}
				b.Hub.Dispatch(evs)
			}
		}
		_ = sub.Close()
	}
}
