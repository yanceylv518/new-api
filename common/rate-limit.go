package common

import (
	"sync"
	"time"
)

type InMemoryRateLimiter struct {
	store               map[string]*[]int64
	successReservations map[string]int
	mutex               sync.Mutex
	expirationDuration  time.Duration
}

func (l *InMemoryRateLimiter) Init(expirationDuration time.Duration) {
	if l.store == nil {
		l.mutex.Lock()
		if l.store == nil {
			l.store = make(map[string]*[]int64)
			l.expirationDuration = expirationDuration
			if expirationDuration > 0 {
				go l.clearExpiredItems()
			}
		}
		l.mutex.Unlock()
	}
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
		l.mutex.Unlock()
	}
}

// ReserveSuccess 预留成功名额；完成回调幂等，失败释放，成功按完成时间计入窗口。
// duration 的单位是秒；不限制时返回 nil 回调。
func (l *InMemoryRateLimiter) ReserveSuccess(key string, maxRequestNum int, duration int64) (func(bool), bool) {
	if maxRequestNum <= 0 || duration <= 0 {
		return nil, true
	}
	l.mutex.Lock()
	now := time.Now().Unix()
	count := 0
	if queue := l.store[key]; queue != nil {
		expired := 0
		for expired < len(*queue) && now-(*queue)[expired] >= duration {
			expired++
		}
		*queue = (*queue)[expired:]
		count = len(*queue)
	}
	if count >= maxRequestNum || l.successReservations[key] >= maxRequestNum-count {
		l.mutex.Unlock()
		return nil, false
	}
	if l.successReservations == nil {
		l.successReservations = make(map[string]int)
	}
	l.successReservations[key]++
	l.mutex.Unlock()
	var once sync.Once
	return func(success bool) {
		once.Do(func() {
			l.mutex.Lock()
			defer l.mutex.Unlock()
			l.successReservations[key]--
			if l.successReservations[key] == 0 {
				delete(l.successReservations, key)
			}
			if success {
				queue := l.store[key]
				if queue == nil {
					queue = &[]int64{}
					l.store[key] = queue
				}
				*queue = append(*queue, time.Now().Unix())
			}
		})
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
