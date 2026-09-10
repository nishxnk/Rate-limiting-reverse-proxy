package limiter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// policyDoc is the wire format of a shared policy.
type policyDoc struct {
	Limit    int   `json:"limit"`
	WindowMS int64 `json:"window_ms"`
	UpdateAt int64 `json:"updated_at_ms"`
}

// PolicyStore shares the active rate-limit policy across every proxy instance
// through Redis. A policy change made on one dashboard is persisted and
// published, and all other instances apply it within a heartbeat.
//
// It is entirely optional: when Redis is down the proxy keeps running with the
// policy it already holds.
type PolicyStore struct {
	client  redis.UniversalClient
	key     string
	channel string
	log     *slog.Logger
}

// NewPolicyStore builds a store using the given key prefix.
func NewPolicyStore(client redis.UniversalClient, prefix string, log *slog.Logger) *PolicyStore {
	if log == nil {
		log = slog.Default()
	}
	return &PolicyStore{
		client:  client,
		key:     prefix + "policy",
		channel: prefix + "policy:updates",
		log:     log,
	}
}

// Load reads the shared policy. ok is false when no policy has been stored yet.
func (s *PolicyStore) Load(ctx context.Context) (p Policy, ok bool, err error) {
	raw, err := s.client.Get(ctx, s.key).Bytes()
	if errors.Is(err, redis.Nil) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var doc policyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Policy{}, false, fmt.Errorf("policy store: corrupt document: %w", err)
	}
	return Policy{Limit: doc.Limit, Window: time.Duration(doc.WindowMS) * time.Millisecond}.Normalize(), true, nil
}

// Save persists the policy and notifies the other instances.
func (s *PolicyStore) Save(ctx context.Context, p Policy) error {
	p = p.Normalize()
	buf, err := json.Marshal(policyDoc{
		Limit:    p.Limit,
		WindowMS: p.Window.Milliseconds(),
		UpdateAt: time.Now().UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("policy store: marshal: %w", err)
	}
	if err := s.client.Set(ctx, s.key, buf, 0).Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := s.client.Publish(ctx, s.channel, buf).Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Watch applies remote policy updates until ctx is cancelled. It reconnects on
// its own, so a Redis restart does not stop propagation.
func (s *PolicyStore) Watch(ctx context.Context, apply func(Policy)) {
	sub := s.client.Subscribe(ctx, s.channel)
	defer func() {
		if err := sub.Close(); err != nil && ctx.Err() == nil {
			s.log.Debug("policy subscription close failed", "error", err)
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
			var doc policyDoc
			if err := json.Unmarshal([]byte(msg.Payload), &doc); err != nil {
				s.log.Warn("ignoring malformed policy broadcast", "error", err)
				continue
			}
			p := Policy{Limit: doc.Limit, Window: time.Duration(doc.WindowMS) * time.Millisecond}.Normalize()
			apply(p)
		}
	}
}
