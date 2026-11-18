package outboundproxy

import (
	"context"
	"time"
)

func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sub.done != nil || m.sub.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, wake := make(chan struct{}), make(chan struct{}, 1)
	m.sub.stop, m.sub.done, m.sub.wake = cancel, done, wake
	m.scheduleLocked(time.Now())
	go m.runSubscriptionSchedule(ctx, done, wake)
}

func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	m.sub.stopped = true
	m.sub.next = time.Time{}
	m.invalidatePreviewLocked()
	if m.sub.stop != nil {
		m.sub.stop()
	}
	if m.sub.workCancel != nil {
		m.sub.workCancel()
	}
	done, work := m.sub.done, m.sub.workDone
	m.mu.Unlock()
	for _, ch := range []chan struct{}{done, work} {
		if ch == nil {
			continue
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *Manager) scheduleLocked(next time.Time) {
	m.sub.next = time.Time{}
	if m.sub.wake != nil && !m.sub.stopped && m.sub.record.Mode == "subscription" && m.sub.record.AutoRefresh {
		m.sub.next = next
	}
	m.wakeSubscriptionLocked()
}

func (m *Manager) wakeSubscriptionLocked() {
	select {
	case m.sub.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) runSubscriptionSchedule(ctx context.Context, done, wake chan struct{}) {
	defer close(done)
	for ctx.Err() == nil {
		m.mu.RLock()
		next, busy := m.sub.next, m.sub.workDone != nil
		m.mu.RUnlock()
		if next.IsZero() || busy {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
			_, _ = m.refreshSubscription(ctx, true)
		}
	}
}
