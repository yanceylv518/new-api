package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenConcurrencyRealRedis(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR is not configured")
	}
	a := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	b := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx := context.Background()
	tokenID := int(time.Now().UnixNano())
	first, err := AcquireTokenConcurrency(ctx, a, tokenID, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	_, err = AcquireTokenConcurrency(ctx, b, tokenID, 1)
	assert.ErrorIs(t, err, ErrTokenConcurrencyExceeded)
	alive, err := first.run(ctx, "renew", 0)
	require.NoError(t, err)
	assert.True(t, alive)
	require.NoError(t, first.Close())
	next, err := AcquireTokenConcurrency(ctx, b, tokenID, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = next.Close() })
	require.NoError(t, first.Close())
	_, err = AcquireTokenConcurrency(ctx, a, tokenID, 1)
	assert.ErrorIs(t, err, ErrTokenConcurrencyExceeded, "late release cannot remove another instance's lease")
}

func tokenConcurrencyTestClients(t *testing.T) (*miniredis.Miniredis, *redis.Client, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	a := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	b := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return server, a, b
}

func TestTokenConcurrencySharedCapacity(t *testing.T) {
	_, a, b := tokenConcurrencyTestClients(t)
	type result struct {
		lease *TokenConcurrencyLease
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, client := range []*redis.Client{a, b} {
		go func(client *redis.Client) {
			<-start
			lease, err := AcquireTokenConcurrency(context.Background(), client, 42, 1)
			results <- result{lease, err}
		}(client)
	}
	close(start)
	var winner *TokenConcurrencyLease
	denied := 0
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				assert.ErrorIs(t, result.err, ErrTokenConcurrencyExceeded)
				denied++
			} else {
				require.Nil(t, winner, "independent clients must not both acquire the last slot")
				winner = result.lease
				t.Cleanup(func() { _ = result.lease.Close() })
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent acquisitions did not finish")
		}
	}
	require.NotNil(t, winner)
	assert.Equal(t, 1, denied)
	allowed, err := winner.run(context.Background(), "acquire", 1)
	require.NoError(t, err)
	assert.True(t, allowed, "transport retries with the same lease ID are idempotent")
	other, err := AcquireTokenConcurrency(context.Background(), b, 43, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	require.NoError(t, winner.Close())
	next, err := AcquireTokenConcurrency(context.Background(), b, 42, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = next.Close() })
	require.NoError(t, winner.Close())
	_, err = AcquireTokenConcurrency(context.Background(), a, 42, 1)
	assert.ErrorIs(t, err, ErrTokenConcurrencyExceeded, "duplicate release cannot remove another request's slot")
}

func TestTokenConcurrencyRenewalAndExpiry(t *testing.T) {
	server, a, b := tokenConcurrencyTestClients(t)
	now := time.Now()
	server.SetTime(now)
	lease, err := AcquireTokenConcurrency(context.Background(), a, 42, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })
	server.SetTime(now.Add(tokenConcurrencyLeaseTTL - time.Second))
	alive, err := lease.run(context.Background(), "renew", 0)
	require.NoError(t, err)
	assert.True(t, alive)
	server.SetTime(now.Add(tokenConcurrencyLeaseTTL + time.Second))
	_, err = AcquireTokenConcurrency(context.Background(), b, 42, 1)
	assert.ErrorIs(t, err, ErrTokenConcurrencyExceeded, "renewal retains capacity beyond the original expiry")
	server.SetTime(now.Add(2 * tokenConcurrencyLeaseTTL))
	alive, err = lease.run(context.Background(), "renew", 0)
	require.NoError(t, err)
	assert.False(t, alive, "an expired lease must not be resurrected")
	next, err := AcquireTokenConcurrency(context.Background(), b, 42, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = next.Close() })
	require.NoError(t, lease.Close())
	_, err = AcquireTokenConcurrency(context.Background(), a, 42, 1)
	assert.ErrorIs(t, err, ErrTokenConcurrencyExceeded, "late cleanup must not release the replacement lease")
	server.FastForward(tokenConcurrencyLeaseTTL + time.Second)
	assert.False(t, server.Exists(next.key), "crashed instances must not leave permanent counters")
}

func TestTokenConcurrencyRenewalFailureCancelsRequest(t *testing.T) {
	for _, redisFailure := range []bool{false, true} {
		name := "lost_member"
		if redisFailure {
			name = "redis_unavailable"
		}
		t.Run(name, func(t *testing.T) {
			server, client, _ := tokenConcurrencyTestClients(t)
			lease, err := AcquireTokenConcurrency(context.Background(), client, 42, 1)
			require.NoError(t, err)
			t.Cleanup(func() { server.SetError(""); _ = lease.Close() })
			// Replace the timer source with a controlled tick, not a short TTL
			// or a sleep that makes this test depend on machine scheduling.
			lease.stopRenewal()
			<-lease.renewalDone
			if redisFailure {
				server.SetError("ERR test unavailable")
			} else {
				require.NoError(t, client.Del(context.Background(), lease.key).Err())
			}
			ticks := make(chan time.Time, 1)
			ticks <- time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lease.keepAlive(ctx, ticks)
			assert.ErrorIs(t, lease.Context().Err(), context.Canceled)
			if !redisFailure {
				assert.ErrorIs(t, context.Cause(lease.Context()), ErrTokenConcurrencyLeaseLost)
			}
		})
	}
}

func TestTokenConcurrencyClientCancellationDoesNotReleaseEarly(t *testing.T) {
	_, a, b := tokenConcurrencyTestClients(t)
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := AcquireTokenConcurrency(ctx, a, 42, 1)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); _ = lease.Close() })
	cancel()
	assert.ErrorIs(t, lease.Context().Err(), context.Canceled)
	alive, err := lease.run(context.Background(), "renew", 0)
	require.NoError(t, err)
	assert.True(t, alive, "canceled handlers still unwinding retain their slot")
	_, err = AcquireTokenConcurrency(context.Background(), b, 42, 1)
	assert.ErrorIs(t, err, ErrTokenConcurrencyExceeded)
	require.NoError(t, lease.Close())
	next, err := AcquireTokenConcurrency(context.Background(), b, 42, 1)
	require.NoError(t, err)
	require.NoError(t, next.Close())
	_, err = AcquireTokenConcurrency(ctx, a, 42, 1)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = AcquireTokenConcurrency(context.Background(), nil, 42, 1)
	assert.Error(t, err)
}
