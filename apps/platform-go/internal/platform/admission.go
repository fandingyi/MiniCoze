package platform

import "sync"

// Entries exist only while requests are active, bounding memory by admission.
type channelAdmission struct {
	mu     sync.Mutex
	active map[string]int
	limit  int
}

func (a *channelAdmission) acquire(channel string) (func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[channel] >= a.limit {
		return nil, false
	}
	a.active[channel]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.active[channel]--
			if a.active[channel] == 0 {
				delete(a.active, channel)
			}
		})
	}, true
}
