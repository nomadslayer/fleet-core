// Package bus is the fan-out notification layer: "machine X's desired
// state changed, wake its stream". Payloads are never carried on the
// bus — subscribers re-read authoritative state from the Store. That
// keeps the bus contract trivial and makes NATS a drop-in replacement
// for multi-instance control planes.
package bus

import "sync"

type Bus interface {
	// Publish signals subscribers of topic. Non-blocking.
	Publish(topic string)
	// Subscribe returns a signal channel and a cancel func.
	Subscribe(topic string) (<-chan struct{}, func())
}

type InProc struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewInProc() *InProc {
	return &InProc{subs: map[string]map[chan struct{}]struct{}{}}
}

func (b *InProc) Publish(topic string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[topic] {
		select {
		case ch <- struct{}{}:
		default: // subscriber already has a pending signal
		}
	}
}

func (b *InProc) Subscribe(topic string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subs[topic] == nil {
		b.subs[topic] = map[chan struct{}]struct{}{}
	}
	b.subs[topic][ch] = struct{}{}
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		delete(b.subs[topic], ch)
		if len(b.subs[topic]) == 0 {
			delete(b.subs, topic)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}
