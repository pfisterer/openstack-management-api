package assistant

import (
	"sync"
	"time"
)

// Limiter bounds the load the assistant puts on the platform's GPUs: questions per person and hour, and questions running at once over everyone. It counts per pod (in memory); with several replicas the limits apply per replica, which is enough to keep one person or a burst from filling the model, not an accounting.
type Limiter struct {
	PerHour    int
	Concurrent int
	Now        func() time.Time

	mu      sync.Mutex
	running int
	asked   map[string][]time.Time
}

// NewLimiter is a limiter with perHour questions per person and hour and concurrent questions at once; 0 means no bound of that kind.
func NewLimiter(perHour, concurrent int) *Limiter {
	return &Limiter{PerHour: perHour, Concurrent: concurrent, asked: map[string][]time.Time{}}
}

// Acquire takes a slot for who, or says why not: "busy" (too many at once, retry soon) or "quota" (this person's questions of the last hour are used up, retry after the duration).
func (l *Limiter) Acquire(who string) (release func(), reason string, retryAfter time.Duration) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	t := now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Concurrent > 0 && l.running >= l.Concurrent {
		return nil, "busy", 10 * time.Second
	}
	if l.PerHour > 0 {
		kept := l.asked[who][:0]
		for _, at := range l.asked[who] {
			if t.Sub(at) < time.Hour {
				kept = append(kept, at)
			}
		}
		l.asked[who] = kept
		if len(kept) >= l.PerHour {
			return nil, "quota", time.Hour - t.Sub(kept[0])
		}
		l.asked[who] = append(kept, t)
	}
	l.running++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.running--
			l.mu.Unlock()
		})
	}, "", 0
}
