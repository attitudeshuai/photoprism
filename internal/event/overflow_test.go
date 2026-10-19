package event

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drain collects buffered messages without blocking.
func drain(s Subscription) int {
	received := 0

	for {
		select {
		case <-s.Receiver:
			received++
		default:
			return received
		}
	}
}

func TestSubscribeCapRejectsInvalidValues(t *testing.T) {
	h := NewHub()

	for _, capacity := range []int{0, -1, MinCap - 1, MaxCap + 1, int(^uint(0) >> 1)} {
		_, err := h.SubscribeCap(capacity, "a.b")
		require.ErrorIs(t, err, ErrInvalidCapacity, "capacity %d must be rejected", capacity)
	}

	assert.Panics(t, func() { h.NonBlockingSubscribe(0, "a.b") })

	_, err := h.SubscribeCap(DefaultCap)
	require.NoError(t, err)

	_, err = h.SubscribeKind("kind-that-does-not-exist", "a.b")
	require.ErrorIs(t, err, ErrUnknownKind)
}

func TestSetCapacityValidatesKindAndCapacity(t *testing.T) {
	require.ErrorIs(t, SetCapacity("", DefaultCap), ErrUnknownKind)
	require.ErrorIs(t, SetCapacity("test-empty-kind", 0), ErrInvalidCapacity)
	require.ErrorIs(t, SetCapacity("test-empty-kind", MaxCap+1), ErrInvalidCapacity)

	require.NoError(t, SetCapacity("test-sized-kind", 2))

	capacity, ok := Capacity("test-sized-kind")
	require.True(t, ok)
	assert.Equal(t, 2, capacity)

	h := NewHub()
	s, err := h.SubscribeKind("test-sized-kind", "sized.event")
	require.NoError(t, err)
	defer h.Unsubscribe(s)

	assert.Equal(t, 2, cap(s.Receiver))
}

func TestOverflowRecordsDropsPerTopic(t *testing.T) {
	h := NewHub()

	s, err := h.SubscribeCap(1, "photos.*", "albums.*")
	require.NoError(t, err)

	defer h.Unsubscribe(s)

	notify := s.LagNotify()

	h.Publish(Message{Name: "photos.updated", Fields: Data{"n": 1}}) // Buffered.
	h.Publish(Message{Name: "photos.updated"})                       // Dropped.
	h.Publish(Message{Name: "photos.created"})                       // Dropped.
	h.Publish(Message{Name: "albums.updated"})                       // Dropped.
	h.Publish(Message{Name: "count.photos"})                         // No matching pattern.

	stats := s.DropStats()

	assert.Equal(t, int64(3), stats.Total)
	assert.Equal(t, map[string]int64{"photos.updated": 1, "photos.created": 1, "albums.updated": 1}, stats.ByTopic)
	assert.Equal(t, "photos.updated", stats.FirstTopic)
	assert.False(t, stats.FirstAt.IsZero())
	assert.False(t, stats.LastAt.IsZero())

	// The lag signal closes with the first lost event.
	select {
	case <-notify:
	default:
		t.Fatal("lag notification was not signaled on overflow")
	}

	// Hub-wide ledger counts the same drops and ignores unmatched topics.
	hubStats := h.Stats()
	assert.Equal(t, int64(3), hubStats.Dropped)
	assert.Equal(t, int64(3), hubStats.ByTopic["photos.updated"]+hubStats.ByTopic["photos.created"]+hubStats.ByTopic["albums.updated"])
	assert.NotContains(t, hubStats.ByTopic, "count.photos")

	require.Len(t, hubStats.Subscribers, 1)
	assert.Equal(t, "default", hubStats.Subscribers[0].Kind)
	assert.Equal(t, 1, hubStats.Subscribers[0].Capacity)
	assert.Equal(t, int64(3), hubStats.Subscribers[0].Total)

	// One buffered event is still delivered, so published equals delivered plus dropped.
	assert.Equal(t, 1, drain(s))

	// Acknowledging the observed total rearms the signal until the next overflow.
	s.AckLag(stats.Total)
	select {
	case <-s.LagNotify():
		t.Fatal("lag notification should stay open after acknowledging the current total")
	default:
	}

	h.Publish(Message{Name: "photos.updated"}) // Buffered.
	h.Publish(Message{Name: "photos.deleted"}) // Dropped.

	select {
	case <-s.LagNotify():
	default:
		t.Fatal("lag notification was not signaled on the next overflow")
	}

	assert.Equal(t, int64(4), s.DroppedTotal())

	// An acknowledge that lags behind the real total immediately signals again.
	s.AckLag(stats.Total)

	select {
	case <-s.LagNotify():
	default:
		t.Fatal("lag notification must signal immediately when drops happened before the ack")
	}
}

func TestNoOverflowPreservesOrderAndContent(t *testing.T) {
	h := NewHub()

	s, err := h.SubscribeCap(64, "photos.*")
	require.NoError(t, err)

	defer h.Unsubscribe(s)

	const total = 50

	for i := 0; i < total; i++ {
		h.Publish(Message{Name: "photos.updated", Fields: Data{"seq": i}})
	}

	for i := 0; i < total; i++ {
		select {
		case msg := <-s.Receiver:
			assert.Equal(t, "photos.updated", msg.Name)
			assert.Equal(t, i, msg.Fields["seq"])
		case <-time.After(time.Second):
			t.Fatalf("did not receive message %d", i)
		}
	}

	stats := s.DropStats()
	assert.Equal(t, int64(0), stats.Total)
	assert.Empty(t, stats.ByTopic)
	assert.Equal(t, "", stats.FirstTopic)
}

func TestWildcardMatching(t *testing.T) {
	h := NewHub()

	s, err := h.SubscribeCap(16, "photos.*", "a.x")
	require.NoError(t, err)

	defer h.Unsubscribe(s)

	h.Publish(Message{Name: "photos.updated"})
	h.Publish(Message{Name: "a.x"})
	h.Publish(Message{Name: "photos.updated.extra"})
	h.Publish(Message{Name: "other.updated"})

	assert.Equal(t, 2, drain(s))
	assert.Equal(t, int64(0), s.DroppedTotal())
}

func TestOverlappingPatternsDeliverOnce(t *testing.T) {
	h := NewHub()

	s, err := h.SubscribeCap(8, "a.*", "a.x")
	require.NoError(t, err)

	defer h.Unsubscribe(s)

	h.Publish(Message{Name: "a.x"})

	assert.Equal(t, 1, drain(s))
}

func TestUnsubscribeStopsDeliveryAndKeepsLedger(t *testing.T) {
	h := NewHub()

	s, err := h.SubscribeCap(1, "a.b")
	require.NoError(t, err)

	h.Publish(Message{Name: "a.b"})
	h.Publish(Message{Name: "a.b"})

	require.Equal(t, int64(1), s.DroppedTotal())

	h.Unsubscribe(s)
	h.Unsubscribe(s) // Repeated unsubscribe must be safe.

	// Publishing after unsubscribe must not block or panic.
	h.Publish(Message{Name: "a.b"})

	// A closed buffered channel still delivers its queued message, so drain before expecting close.
	closed := false

	for {
		select {
		case _, ok := <-s.Receiver:
			if !ok {
				closed = true
			}
		case <-time.After(time.Second):
			t.Fatal("receiver channel was not closed after unsubscribe")
		}

		if closed {
			break
		}
	}

	assert.True(t, closed)

	// Cumulative operator counters survive the ended subscription.
	assert.Equal(t, int64(1), h.Stats().Dropped)
}

func TestHubCloseClosesAllReceivers(t *testing.T) {
	h := NewHub()

	s, err := h.SubscribeCap(4, "a.b")
	require.NoError(t, err)

	h.Close()

	select {
	case _, ok := <-s.Receiver:
		assert.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("receiver channel was not closed when the hub closed")
	}
}

func TestZeroValueSubscriptionIsSafe(t *testing.T) {
	var s Subscription

	assert.Equal(t, int64(0), s.DroppedTotal())
	assert.Empty(t, s.DroppedByTopic())
	assert.NotPanics(t, func() { s.AckLag(0) })
}

func TestConcurrentPublishesDropsAreCountedExactly(t *testing.T) {
	h := NewHub()

	s, err := h.SubscribeCap(1, "a.*", "b.*")
	require.NoError(t, err)

	defer h.Unsubscribe(s)

	const publishers = 8
	const perPublisher = 500

	published := int64(0)
	var publishMu sync.Mutex

	var wg sync.WaitGroup

	// Concurrent short-lived subscriptions and unsubscriptions race with publishing.
	for churn := 0; churn < 4; churn++ {
		go func() {
			for i := 0; i < 50; i++ {
				temp, subErr := h.SubscribeCap(2, "c.*")
				if subErr != nil {
					t.Error(subErr)
					return
				}

				time.Sleep(time.Microsecond)
				h.Unsubscribe(temp)
			}
		}()
	}

	for g := 0; g < publishers; g++ {
		wg.Add(1)

		go func(id int) {
			defer wg.Done()

			for i := 0; i < perPublisher; i++ {
				topic := "a.x"
				if (id+i)%3 == 0 {
					topic = "b.x"
				}

				h.Publish(Message{Name: topic})
				publishMu.Lock()
				published++
				publishMu.Unlock()
			}
		}(g)
	}

	wg.Wait()

	// Every attempt either buffered or was recorded as dropped exactly once.
	delivered := drain(s)

	stats := s.DropStats()

	assert.Equal(t, published, int64(delivered)+stats.Total,
		"every published event must be delivered or counted once")

	var topicSum int64

	for _, count := range stats.ByTopic {
		topicSum += count
	}

	assert.Equal(t, stats.Total, topicSum, "per-topic counts must add up to the total")

	hubStats := h.Stats()
	assert.Equal(t, stats.Total, hubStats.Dropped, "hub ledger must match the subscription ledger")
}
