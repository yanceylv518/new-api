package common

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Redis 使用原子固定窗口；窗口边界仍允许两倍瞬时突发，保持现有限流语义。
const redisFixedWindowScript = `
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
end
local ttl = redis.call('TTL', KEYS[1])
if ttl < 0 then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
  ttl = redis.call('TTL', KEYS[1])
end
if count > tonumber(ARGV[1]) then
  return {0, count, ttl}
end
return {1, count, ttl}
`

type RateLimitError struct {
	RetryAfterSeconds int64
}

func (err *RateLimitError) Error() string {
	return "rate limit exceeded"
}

var scopedRateLimiter InMemoryRateLimiter

// CheckRateLimit 占用独立限流额度，不负责凭据校验或修改认证状态。
func CheckRateLimit(ctx context.Context, key string, maximum int, duration int64) error {
	if key == "" || maximum <= 0 || duration <= 0 {
		return errors.New("invalid rate limit configuration")
	}
	if RedisEnabled {
		allowed, _, ttl, err := RedisFixedWindowTake(ctx, key, maximum, duration)
		if err != nil {
			return err
		}
		if !allowed {
			return &RateLimitError{RetryAfterSeconds: max(ttl, 1)}
		}
		return nil
	}
	scopedRateLimiter.Init(RateLimitKeyExpirationDuration)
	if !scopedRateLimiter.Request(key, maximum, duration) {
		return &RateLimitError{RetryAfterSeconds: duration}
	}
	return nil
}

func RedisFixedWindowTake(ctx context.Context, key string, maxRequestNum int, duration int64) (bool, int64, int64, error) {
	if RDB == nil {
		return false, 0, 0, errors.New("Redis client is not initialized")
	}
	if key == "" {
		return false, 0, 0, errors.New("rate limit key is empty")
	}
	if maxRequestNum <= 0 {
		return false, 0, 0, errors.New("rate limit maximum must be positive")
	}
	if duration <= 0 {
		return false, 0, 0, errors.New("rate limit duration must be positive")
	}

	values, err := RDB.Eval(ctx, redisFixedWindowScript, []string{key}, maxRequestNum, duration).Slice()
	if err != nil {
		return false, 0, 0, err
	}
	if len(values) != 3 {
		return false, 0, 0, fmt.Errorf("unexpected Redis rate limit reply length %d", len(values))
	}
	allowedValue, err := redisReplyInteger(values[0])
	if err != nil {
		return false, 0, 0, err
	}
	count, err := redisReplyInteger(values[1])
	if err != nil {
		return false, 0, 0, err
	}
	ttlSeconds, err := redisReplyInteger(values[2])
	if err != nil {
		return false, 0, 0, err
	}
	return allowedValue == 1, count, ttlSeconds, nil
}

func redisReplyInteger(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected Redis integer reply type %T", value)
	}
}

// rateLimitRequest stores one accepted request in a key's sliding window.
type rateLimitRequest struct {
	next      *rateLimitRequest
	timestamp int64
}

// rateLimitQueue keeps accepted requests ordered from oldest to newest.
type rateLimitQueue struct {
	head   *rateLimitRequest
	tail   *rateLimitRequest
	length int
}

// append adds an accepted request without preallocating for the configured limit.
func (q *rateLimitQueue) append(timestamp int64) {
	request := &rateLimitRequest{timestamp: timestamp}
	if q.tail == nil {
		q.head = request
		q.tail = request
	} else {
		q.tail.next = request
		q.tail = request
	}
	q.length++
}

// removeExpired releases every expired request at the front of the queue.
func (q *rateLimitQueue) removeExpired(now int64, duration int64) {
	if q.head == nil || now-q.head.timestamp < duration {
		return
	}

	// Requests are time ordered, so an expired tail means the whole queue expired.
	if now-q.tail.timestamp >= duration {
		q.clear()
		return
	}

	for now-q.head.timestamp >= duration {
		expired := q.head
		q.head = expired.next
		expired.next = nil
		q.length--
	}
}

// clear releases the whole request chain when its key expires.
func (q *rateLimitQueue) clear() {
	q.head = nil
	q.tail = nil
	q.length = 0
}

// rateLimitEntry is both a rate-limit bucket and a node in the key-level LRU.
type rateLimitEntry struct {
	lastActive    time.Time
	windowSeconds int64
	element       *list.Element
	requests      rateLimitQueue
	key           string
}

// InMemoryRateLimiter implements a sliding-window limiter with idle-key eviction.
// Reservations are tracked apart from the LRU so an in-flight request survives
// its key being evicted while it is still running.
type InMemoryRateLimiter struct {
	store              map[string]*rateLimitEntry
	lru                *list.List
	reservations       map[string]int
	mutex              sync.Mutex
	expirationDuration time.Duration
}

// Init initializes the limiter once. Repeated calls leave the first configuration unchanged.
func (l *InMemoryRateLimiter) Init(expirationDuration time.Duration) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	if l.store != nil {
		return
	}

	l.store = make(map[string]*rateLimitEntry)
	l.lru = list.New()
	l.reservations = make(map[string]int)
	l.expirationDuration = expirationDuration
	if expirationDuration > 0 {
		go l.clearExpiredItems(time.NewTicker(expirationDuration).C)
	}
}

// clearExpiredItems periodically removes expired entries from the LRU tail.
func (l *InMemoryRateLimiter) clearExpiredItems(ticks <-chan time.Time) {
	for now := range ticks {
		l.deleteExpiredEntries(now)
	}
}

// deleteExpiredEntries walks only the oldest LRU entries and stops at the first active key.
func (l *InMemoryRateLimiter) deleteExpiredEntries(now time.Time) {
	if l.expirationDuration <= 0 {
		return
	}

	l.mutex.Lock()
	defer l.mutex.Unlock()

	for oldest := l.lru.Back(); oldest != nil; {
		next := oldest.Prev()
		entry := oldest.Value.(*rateLimitEntry)
		if now.Sub(entry.lastActive) < l.expirationDuration {
			return
		}
		// 长窗口计数仍有效时保留该项，继续回收后面的短窗口空闲项。
		if now.Unix()-entry.lastActive.Unix() < entry.windowSeconds {
			oldest = next
			continue
		}

		delete(l.store, entry.key)
		l.lru.Remove(oldest)
		entry.element = nil
		entry.requests.clear()
		oldest = next
	}
}

// touchEntry returns the key's bucket, creating it on demand, and marks it as
// the most recently active key. The caller must hold the mutex.
func (l *InMemoryRateLimiter) touchEntry(key string, now time.Time) *rateLimitEntry {
	entry, ok := l.store[key]
	if !ok {
		entry = &rateLimitEntry{
			key:        key,
			lastActive: now,
		}
		entry.element = l.lru.PushFront(entry)
		l.store[key] = entry
		return entry
	}
	entry.lastActive = now
	l.lru.MoveToFront(entry.element)
	return entry
}

// Request parameter duration's unit is seconds
func (l *InMemoryRateLimiter) Request(key string, maxRequestNum int, duration int64) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	now := time.Now()
	entry := l.touchEntry(key, now)
	entry.windowSeconds = duration
	entry.requests.removeExpired(now.Unix(), duration)

	allowed := entry.requests.length < maxRequestNum
	if allowed {
		entry.requests.append(now.Unix())
	}

	return allowed
}

// RateLimitReservation counts an active request towards admission until its
// outcome is known. Failed requests release the slot without consuming quota.
type RateLimitReservation struct {
	limiter       *InMemoryRateLimiter
	key           string
	windowSeconds int64
	once          sync.Once
}

// Reserve admits a request when the key's accepted requests plus its in-flight
// reservations stay below maxRequests. It returns nil when the key is saturated.
func (l *InMemoryRateLimiter) Reserve(key string, maxRequests int, duration int64) *RateLimitReservation {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	now := time.Now()
	entry := l.touchEntry(key, now)
	entry.windowSeconds = duration
	entry.requests.removeExpired(now.Unix(), duration)
	if entry.requests.length+l.reservations[key] >= maxRequests {
		return nil
	}
	l.reservations[key]++
	return &RateLimitReservation{limiter: l, key: key, windowSeconds: duration}
}

// Complete releases the reservation. A successful outcome records the request
// in the key's sliding window; a failed one only frees the slot.
func (r *RateLimitReservation) Complete(success bool) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		l := r.limiter
		l.mutex.Lock()
		defer l.mutex.Unlock()
		l.reservations[r.key]--
		if l.reservations[r.key] == 0 {
			delete(l.reservations, r.key)
		}
		if success {
			now := time.Now()
			entry := l.touchEntry(r.key, now)
			entry.windowSeconds = r.windowSeconds
			entry.requests.append(now.Unix())
		}
	})
}
