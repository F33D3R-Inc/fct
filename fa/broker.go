package fa

import "sync"

// Broker fans event messages out across application instances (audit T1 #1).
// Every Emit*/Broadcast publishes a routing message; each instance's hub
// subscribes and delivers to ITS local connections. The default is in-process
// (single instance). For a multi-instance deployment, supply a Redis/NATS-backed
// Broker via fa.WithBroker — a ~30-line adapter:
//
//	Publish:   PUBLISH "fa" msg
//	Subscribe: SUBSCRIBE "fa"; for each message call fn(msg)
//
// Pub/sub echoes to the publisher too, so an instance receives its own messages
// and applies them locally — one code path for local and remote delivery.
//
// Deployment note: run SSE behind sticky load-balancing so a client's /sse and
// /events land on the same instance (its own connection stays local); the broker
// handles delivery to connections on OTHER instances.
type Broker interface {
	// Publish sends a routing message to all instances (including this one).
	Publish(msg []byte) error
	// Subscribe registers a handler for messages published by any instance.
	Subscribe(fn func(msg []byte))
}

// localBroker is the default single-process Broker: Publish synchronously
// delivers to local subscribers, so single-instance behaviour (and ordering) is
// unchanged.
type localBroker struct {
	mu  sync.RWMutex
	fns []func([]byte)
}

func newLocalBroker() *localBroker { return &localBroker{} }

func (b *localBroker) Publish(msg []byte) error {
	b.mu.RLock()
	fns := b.fns
	b.mu.RUnlock()
	for _, fn := range fns {
		fn(msg)
	}
	return nil
}

func (b *localBroker) Subscribe(fn func([]byte)) {
	b.mu.Lock()
	b.fns = append(b.fns, fn)
	b.mu.Unlock()
}
