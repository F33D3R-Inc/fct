package fa

import (
	"strings"
	"sync"
	"testing"
)

// memBroker is a shared in-memory broker that connects multiple hubs — it stands
// in for Redis/NATS to prove cross-instance fan-out without a real backend.
type memBroker struct {
	mu   sync.Mutex
	subs []func([]byte)
}

func (b *memBroker) Publish(msg []byte) error {
	b.mu.Lock()
	subs := append([]func([]byte){}, b.subs...)
	b.mu.Unlock()
	for _, fn := range subs {
		fn(msg)
	}
	return nil
}

func (b *memBroker) Subscribe(fn func([]byte)) {
	b.mu.Lock()
	b.subs = append(b.subs, fn)
	b.mu.Unlock()
}

// TestCrossInstanceFanout is the T1 #1 proof: a client connected to instance B
// receives an event emitted on instance A, via a shared broker — and a key that
// is shared (T1 #2) so the signature verifies.
func TestCrossInstanceFanout(t *testing.T) {
	key := []byte("shared-signing-key-shared-key!!!")
	broker := &memBroker{}
	hubA := newHub(key, broker, nil, nil) // instance A
	hubB := newHub(key, broker, nil, nil) // instance B

	// A client is connected (and subscribed to "post:9") on instance B only.
	cb := &sseClient{id: newConnID(), channels: make(map[string]bool), send: make(chan []byte, 4)}
	hubB.register(cb)
	hubB.subscribe(cb.id, "post:9")

	// A different client on instance A, NOT subscribed.
	ca := &sseClient{id: newConnID(), channels: make(map[string]bool), send: make(chan []byte, 4)}
	hubA.register(ca)

	// Emit on instance A.
	hubA.EmitChannel("post:9", Event{Op: "replace", FacetID: "Stats:post:9", Fragment: "<b>10</b>"})

	// B's subscriber must receive it (delivered across instances).
	select {
	case line := <-cb.send:
		if !strings.Contains(string(line), "Stats:post:9") {
			t.Fatalf("cross-instance frame wrong: %s", line)
		}
		if !strings.Contains(string(line), `"hmac"`) {
			t.Errorf("event not signed: %s", line)
		}
	default:
		t.Fatal("cross-instance: client on B did not receive event emitted on A")
	}

	// The non-subscribed client on A must receive nothing.
	select {
	case <-ca.send:
		t.Fatal("non-subscriber received a channel event")
	default:
	}
}

// TestSingleInstanceStillWorks confirms the default (nil broker) delivers locally.
func TestSingleInstanceStillWorks(t *testing.T) {
	h := newHub([]byte("k-k-k-k-k-k-k-k-k-k-k-k-k-k-k-k!"), nil, nil, nil)
	c := &sseClient{id: newConnID(), channels: make(map[string]bool), send: make(chan []byte, 2)}
	h.register(c)
	h.EmitConn(c.id, Event{Op: "replace", FacetID: "X", Fragment: "<b>1</b>"})
	select {
	case line := <-c.send:
		if !strings.Contains(string(line), `"facet_id":"X"`) {
			t.Fatalf("local frame wrong: %s", line)
		}
	default:
		t.Fatal("single-instance local delivery failed")
	}
}

func TestSigningKeyFromOption(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	app := New([]byte(`{}`), WithSigningKey(key))
	if app.Key() != "30313233343536373839616263646566"+"30313233343536373839616263646566" {
		t.Fatalf("Key() did not reflect the provided signing key: %s", app.Key())
	}
}
