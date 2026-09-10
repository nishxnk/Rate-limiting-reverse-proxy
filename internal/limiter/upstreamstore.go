package limiter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/redis/go-redis/v9"
)

// UpstreamStore shares the active upstream target across every proxy instance
// through Redis, so a target chosen from one dashboard is adopted by the whole
// fleet and survives a restart. It is entirely optional: with Redis down the
// proxy keeps whatever target it already holds.
//
// The stored value is the raw upstream URL string exactly as an operator typed
// it; interpretation (mock sentinel, http/https validation) stays in the proxy
// package so this store never needs to import it.
type UpstreamStore struct {
	client  redis.UniversalClient
	key     string
	channel string
	log     *slog.Logger
}

// NewUpstreamStore builds a store using the given key prefix.
func NewUpstreamStore(client redis.UniversalClient, prefix string, log *slog.Logger) *UpstreamStore {
	if log == nil {
		log = slog.Default()
	}
	return &UpstreamStore{
		client:  client,
		key:     prefix + "upstream",
		channel: prefix + "upstream:updates",
		log:     log,
	}
}

// Load reads the shared upstream URL. ok is false when none has been stored.
func (s *UpstreamStore) Load(ctx context.Context) (url string, ok bool, err error) {
	v, err := s.client.Get(ctx, s.key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false, nil
	}
	return v, true, nil
}

// Save persists the upstream URL and notifies the other instances.
func (s *UpstreamStore) Save(ctx context.Context, url string) error {
	url = strings.TrimSpace(url)
	if err := s.client.Set(ctx, s.key, url, 0).Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := s.client.Publish(ctx, s.channel, url).Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Watch applies remote upstream changes until ctx is cancelled. It reconnects
// on its own, so a Redis restart does not stop propagation.
func (s *UpstreamStore) Watch(ctx context.Context, apply func(url string)) {
	sub := s.client.Subscribe(ctx, s.channel)
	defer func() {
		if err := sub.Close(); err != nil && ctx.Err() == nil {
			s.log.Debug("upstream subscription close failed", "error", err)
		}
	}()

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, open := <-ch:
			if !open {
				return
			}
			if url := strings.TrimSpace(msg.Payload); url != "" {
				apply(url)
			}
		}
	}
}
