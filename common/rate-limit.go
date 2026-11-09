package common

import (
	"sync"
	"time"
)

type InMemoryRateLimiter struct {
	initOnce           sync.Once
	store              map[string]*[]int64
	successWindows     map[string]*successWindow
	mutex              sync.Mutex
	expirationDuration time.Duration
}

type successWindow struct {
	completed []int64
	pending   int
	duration  int64
}

func (l *InMemoryRateLimiter) Init(expirationDuration time.Duration) {
	l.initOnce.Do(func() {
		l.store = make(map[string]*[]int64)
		l.successWindows = make(map[string]*successWindow)
		l.expirationDuration = expirationDuration
		if expirationDuration > 0 {
			go l.clearExpiredItems()
		}
	})
}

func (l *InMemoryRateLimiter) clearExpiredItems() {
	for {
		time.Sleep(l.expirationDuration)
		l.mutex.Lock()
		now := time.Now().Unix()
		for key := range l.store {
			queue := l.store[key]
			size := len(*queue)
			if size == 0 || now-(*queue)[size-1] > int64(l.expirationDuration.Seconds()) {
				delete(l.store, key)
			}
		}
		for key, window := range l.successWindows {
			n := len(window.completed)
			if window.pending == 0 && (n == 0 || now-window.completed[n-1] >= window.duration) {
				delete(l.successWindows, key)
			}
		}
		l.mutex.Unlock()
	}
}

// ReserveSuccess 预留成功名额，失败后释放，成功按完成时间计入窗口。
// duration 的单位是秒；不限制时返回 nil 回调。
func (l *InMemoryRateLimiter) ReserveSuccess(key string, limit int, duration int64) (func(bool), bool) {
	if limit <= 0 || duration <= 0 {
		return nil, true
	}
	l.mutex.Lock()
	defer l.mutex.Unlock()
	window := l.successWindows[key]
	if window == nil {
		window = &successWindow{}
		l.successWindows[key] = window
	}
	window.duration = duration
	now := time.Now().Unix()
	for len(window.completed) > 0 && now-window.completed[0] >= duration {
		window.completed = window.completed[1:]
	}
	if len(window.completed)+window.pending >= limit {
		return nil, false
	}
	window.pending++
	finished := false
	return func(success bool) {
		l.mutex.Lock()
		defer l.mutex.Unlock()
		if finished {
			return
		}
		finished = true
		window.pending--
		if success {
			window.completed = append(window.completed, time.Now().Unix())
		} else if window.pending == 0 && len(window.completed) == 0 {
			delete(l.successWindows, key)
		}
	}, true
}

// Request 的 duration 参数单位为秒。
func (l *InMemoryRateLimiter) Request(key string, maxRequestNum int, duration int64) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	// [old <-- new]
	queue, ok := l.store[key]
	now := time.Now().Unix()
	if ok {
		if len(*queue) < maxRequestNum {
			*queue = append(*queue, now)
			return true
		} else {
			if now-(*queue)[0] >= duration {
				*queue = (*queue)[1:]
				*queue = append(*queue, now)
				return true
			} else {
				return false
			}
		}
	} else {
		s := make([]int64, 0, maxRequestNum)
		l.store[key] = &s
		*(l.store[key]) = append(*(l.store[key]), now)
	}
	return true
}
