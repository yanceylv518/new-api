package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
)

const (
	tokenConcurrencyLeaseTTL      = 90 * time.Second
	tokenConcurrencyRenewInterval = 20 * time.Second
	tokenConcurrencyRedisTimeout  = 3 * time.Second
)

var (
	ErrTokenConcurrencyExceeded  = errors.New("token concurrency limit exceeded")
	ErrTokenConcurrencyLeaseLost = errors.New("token concurrency lease lost")
)

// v1 is the lease protocol version, not the application release version.
// Every participating instance must share this namespace and Redis database.
var tokenConcurrencyScript = redis.NewScript(`
redis.replicate_commands()
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local exists = redis.call('ZSCORE', KEYS[1], ARGV[2])
if ARGV[1] == 'renew' then
    if not exists then return 0 end
elseif not exists and redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[4]) then
    return 0
end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[3]), ARGV[2])
if redis.call('PTTL', KEYS[1]) < tonumber(ARGV[3]) then
    redis.call('PEXPIRE', KEYS[1], ARGV[3])
end
return 1
`)

type tokenConcurrencyContextKey struct{}

// TokenConcurrencyLimited scopes upstream cancellation changes to requests
// that actually acquired a concurrency lease.
func TokenConcurrencyLimited(ctx context.Context) bool {
	limited, _ := ctx.Value(tokenConcurrencyContextKey{}).(bool)
	return limited
}

type TokenConcurrencyLease struct {
	client         *redis.Client
	key, id        string
	requestContext context.Context
	cancelRequest  context.CancelCauseFunc
	stopRenewal    context.CancelFunc
	renewalDone    chan struct{}
	closeOnce      sync.Once
	closeErr       error
}

// AcquireTokenConcurrency starts renewal on success. The caller must defer
// Close until all request work finishes, including streaming and retries.
func AcquireTokenConcurrency(ctx context.Context, client *redis.Client, tokenID, limit int) (*TokenConcurrencyLease, error) {
	if client == nil {
		return nil, errors.New("token concurrency requires Redis")
	}
	if tokenID <= 0 || limit <= 0 {
		return nil, errors.New("invalid token concurrency policy")
	}
	lease := &TokenConcurrencyLease{
		client: client.WithTimeout(tokenConcurrencyRedisTimeout),
		key:    fmt.Sprintf("new-api:token-concurrency:v1:{%d}", tokenID),
		id:     uuid.NewString(),
	}
	allowed, err := lease.run(ctx, "acquire", limit)
	if err != nil {
		// A timed-out reply may hide a successful write. Remove only our own
		// unique member; TTL remains the fallback if Redis is unreachable.
		_ = lease.release()
		return nil, err
	}
	if !allowed {
		return nil, ErrTokenConcurrencyExceeded
	}
	if err := ctx.Err(); err != nil {
		_ = lease.release()
		return nil, err
	}
	lease.requestContext, lease.cancelRequest = context.WithCancelCause(context.WithValue(ctx, tokenConcurrencyContextKey{}, true))
	// Keep renewing even after client cancellation, until the handler has
	// finished unwinding. Canceling the client must not free a busy slot.
	renewalContext, stop := context.WithCancel(context.Background())
	lease.stopRenewal = stop
	lease.renewalDone = make(chan struct{})
	go func() {
		defer close(lease.renewalDone)
		ticker := time.NewTicker(tokenConcurrencyRenewInterval)
		defer ticker.Stop()
		lease.keepAlive(renewalContext, ticker.C)
	}()
	return lease, nil
}

func (lease *TokenConcurrencyLease) Context() context.Context {
	return lease.requestContext
}

func (lease *TokenConcurrencyLease) run(ctx context.Context, operation string, limit int) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenConcurrencyRedisTimeout)
	defer cancel()
	result, err := tokenConcurrencyScript.Run(ctx, lease.client, []string{lease.key}, operation, lease.id, tokenConcurrencyLeaseTTL.Milliseconds(), limit).Int()
	return result == 1, err
}

// keepAlive accepts ticks separately so renewal failures can be exercised
// deterministically without waiting for production lease durations.
func (lease *TokenConcurrencyLease) keepAlive(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			alive, err := lease.run(ctx, "renew", 0)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				lease.cancelRequest(fmt.Errorf("token concurrency renewal failed: %w", err))
				return
			}
			if !alive {
				lease.cancelRequest(ErrTokenConcurrencyLeaseLost)
				return
			}
		}
	}
}

func (lease *TokenConcurrencyLease) release() error {
	ctx, cancel := context.WithTimeout(context.Background(), tokenConcurrencyRedisTimeout)
	defer cancel()
	// ZREM is idempotent and deletes the set when its last member is removed.
	return lease.client.ZRem(ctx, lease.key, lease.id).Err()
}

func (lease *TokenConcurrencyLease) Close() error {
	lease.closeOnce.Do(func() {
		lease.stopRenewal()
		<-lease.renewalDone
		lease.cancelRequest(context.Canceled)
		lease.closeErr = lease.release()
	})
	return lease.closeErr
}
