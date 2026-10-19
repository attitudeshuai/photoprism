package event

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/photoprism/photoprism/pkg/clean"
)

// Data represents event payload fields.
type Data = map[string]any

// Message represents an emitted event message.
type Message struct {
	Name   string
	Body   []byte
	Fields Data
}

// Topic returns the message topic.
func (m Message) Topic() string {
	return m.Name
}

// TopicSep separates topic hierarchy segments.
const TopicSep = "."

// wildcard matches exactly one topic segment.
const wildcard = "*"

const (
	// DefaultCap is the channel capacity used unless a subscription kind configures another one.
	DefaultCap = 100
	// MinCap is the smallest accepted subscription channel capacity.
	MinCap = 1
	// MaxCap bounds the buffer a single subscription is allowed to allocate.
	MaxCap = 100000
)

// Subscription kind names group subscribers that configure a shared channel capacity.
const (
	// KindDefault is the kind of subscriptions created through Subscribe.
	KindDefault = "default"
	// KindWebsocket is the kind of subscriptions that forward events to websocket clients.
	KindWebsocket = "websocket"
)

var (
	// ErrInvalidCapacity is returned when a capacity is outside MinCap..MaxCap.
	ErrInvalidCapacity = errors.New("invalid subscription channel capacity")
	// ErrUnknownKind is returned when no capacity has been configured for a subscription kind.
	ErrUnknownKind = errors.New("unknown subscription kind")
)

// kindCap holds the configured channel capacity per subscription kind.
var (
	kindMu  sync.RWMutex
	kindCap = map[string]int{
		KindDefault:   DefaultCap,
		KindWebsocket: DefaultCap,
	}
)

// nextSubId numbers subscriptions so operator reports stay readable across short-lived connections.
var nextSubId atomic.Int64

// SetCapacity configures the channel capacity for subscriptions of the given kind. A previously
// unknown kind is registered, so callers can introduce subscription types without touching this
// package. It rejects empty kind names and capacities outside MinCap..MaxCap.
func SetCapacity(kind string, capacity int) error {
	if kind == "" {
		return fmt.Errorf("%w: kind must not be empty", ErrUnknownKind)
	} else if err := validateCapacity(capacity); err != nil {
		return err
	}

	kindMu.Lock()
	defer kindMu.Unlock()

	kindCap[kind] = capacity

	return nil
}

// Capacity returns the configured capacity of a subscription kind.
func Capacity(kind string) (capacity int, ok bool) {
	kindMu.RLock()
	defer kindMu.RUnlock()

	capacity, ok = kindCap[kind]

	return capacity, ok
}

// validateCapacity rejects channel capacities that would block forever or allocate unbounded memory.
func validateCapacity(capacity int) error {
	if capacity < MinCap || capacity > MaxCap {
		return fmt.Errorf("%w: %d is outside %d..%d", ErrInvalidCapacity, capacity, MinCap, MaxCap)
	}

	return nil
}

// Hub is an in-process publish/subscribe hub with wildcard topic matching.
type Hub struct {
	mu   sync.RWMutex
	subs map[*subscription]struct{}

	// statMu guards the cumulative drop ledger, which survives unsubscribe and close.
	statMu  sync.Mutex
	dropped int64
	byTopic map[string]int64
}

// NewHub creates a new empty event hub instance.
func NewHub() *Hub {
	return &Hub{
		subs:    make(map[*subscription]struct{}),
		byTopic: make(map[string]int64),
	}
}

// sharedHub is the process-wide event hub.
var sharedHub = NewHub()

// SharedHub returns the process-wide shared event hub.
func SharedHub() *Hub {
	return sharedHub
}

// Publish sends a message to every subscription whose topic patterns match. Matching and delivery
// semantics are unchanged when no subscriber overflows: each matching subscriber keeps message
// order, content, and timing; a full subscriber is skipped instead of blocking the publisher.
func (h *Hub) Publish(m Message) {
	words := strings.Split(m.Name, TopicSep)

	h.mu.RLock()
	defer h.mu.RUnlock()

	for s := range h.subs {
		if s.matches(words) {
			s.set(m)
		}
	}
}

// subscribe creates a non-blocking subscription of the given kind and channel capacity.
func (h *Hub) subscribe(kind string, capacity int, topics []string) (Subscription, error) {
	if err := validateCapacity(capacity); err != nil {
		return Subscription{}, err
	}

	patterns := make([][]string, len(topics))

	for i, topic := range topics {
		patterns[i] = strings.Split(topic, TopicSep)
	}

	s := &subscription{
		id:       nextSubId.Add(1),
		kind:     kind,
		topics:   append([]string(nil), topics...),
		patterns: patterns,
		capacity: capacity,
		created:  time.Now(),
		ch:       make(chan Message, capacity),
		lag:      make(chan struct{}),
		hub:      h,
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.subs[s] = struct{}{}

	return Subscription{Topics: s.topics, Receiver: s.ch, sub: s}, nil
}

// NonBlockingSubscribe creates a non-blocking subscription with an explicit channel capacity. It
// panics on invalid capacities, since wiring errors must fail fast at startup; callers that read
// configuration should use SubscribeKind instead.
func (h *Hub) NonBlockingSubscribe(capacity int, topics ...string) Subscription {
	s, err := h.subscribe(KindDefault, capacity, topics)
	if err != nil {
		panic(err)
	}

	return s
}

// SubscribeKind creates a non-blocking subscription using the capacity configured for the kind.
func (h *Hub) SubscribeKind(kind string, topics ...string) (Subscription, error) {
	capacity, ok := Capacity(kind)
	if !ok {
		return Subscription{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}

	return h.subscribe(kind, capacity, topics)
}

// SubscribeCap creates a non-blocking subscription with an explicit channel capacity.
func (h *Hub) SubscribeCap(capacity int, topics ...string) (Subscription, error) {
	return h.subscribe(KindDefault, capacity, topics)
}

// unsubscribe removes the subscription and closes its channel. It waits for in-flight deliveries,
// so a channel is never closed while a publisher sends into it.
func (h *Hub) unsubscribe(s Subscription) {
	sub := s.sub

	if sub == nil || sub.hub != h {
		return
	}

	h.mu.Lock()

	if _, ok := h.subs[sub]; !ok {
		h.mu.Unlock()
		return
	}

	delete(h.subs, sub)
	h.mu.Unlock()

	sub.close()
}

// Unsubscribe removes and closes a subscription on the shared hub.
func (h *Hub) Unsubscribe(s Subscription) {
	h.unsubscribe(s)
}

// Close unsubscribes and closes all subscriptions of the hub.
func (h *Hub) Close() {
	h.mu.Lock()

	subs := make([]*subscription, 0, len(h.subs))

	for s := range h.subs {
		delete(h.subs, s)
		subs = append(subs, s)
	}

	h.mu.Unlock()

	for _, s := range subs {
		s.close()
	}
}

// addDrop records one lost event in the hub-wide cumulative ledger.
func (h *Hub) addDrop(topic string) {
	h.statMu.Lock()
	defer h.statMu.Unlock()

	h.dropped++
	h.byTopic[topic]++
}

// Stats returns the cumulative drop ledger and a snapshot of every live subscription.
func (h *Hub) Stats() HubStats {
	h.statMu.Lock()

	stats := HubStats{
		Dropped: h.dropped,
		ByTopic: make(map[string]int64, len(h.byTopic)),
	}

	for topic, count := range h.byTopic {
		stats.ByTopic[topic] = count
	}

	h.statMu.Unlock()

	h.mu.RLock()
	stats.Subscribers = make([]SubscriberStats, 0, len(h.subs))

	for s := range h.subs {
		stats.Subscribers = append(stats.Subscribers, SubscriberStats{
			ID:        s.id,
			Kind:      s.kind,
			Topics:    append([]string(nil), s.topics...),
			Capacity:  s.capacity,
			CreatedAt: s.created,
			DropStats: s.snapshot(),
		})
	}

	h.mu.RUnlock()

	sort.Slice(stats.Subscribers, func(i, j int) bool {
		return stats.Subscribers[i].ID < stats.Subscribers[j].ID
	})

	return stats
}

// Subscription is a handle to one subscriber's topic subscriptions.
type Subscription struct {
	// Topics lists the topic patterns of the subscription.
	Topics []string
	// Receiver delivers the messages that did not overflow the buffer.
	Receiver <-chan Message

	sub *subscription
}

// DroppedTotal returns the number of events lost by this subscription.
func (s Subscription) DroppedTotal() int64 {
	if s.sub == nil {
		return 0
	}

	return s.sub.snapshot().Total
}

// DroppedByTopic returns a copy of the per-topic drop counts of this subscription.
func (s Subscription) DroppedByTopic() map[string]int64 {
	if s.sub == nil {
		return map[string]int64{}
	}

	return s.sub.snapshot().ByTopic
}

// DropStats returns the full drop ledger of this subscription.
func (s Subscription) DropStats() DropStats {
	if s.sub == nil {
		return DropStats{ByTopic: map[string]int64{}}
	}

	return s.sub.snapshot()
}

// LagNotify returns a channel that is closed once after events were lost since the last AckLag.
// It allows a forwarding goroutine to wake immediately on overflow without polling the counters.
func (s Subscription) LagNotify() <-chan struct{} {
	if s.sub == nil {
		return nil
	}

	return s.sub.lagChannel()
}

// AckLag rearms lag notification after the caller observed the given drop total. If further drops
// occurred in the meantime, the next notification channel is already closed.
func (s Subscription) AckLag(seenTotal int64) {
	if s.sub != nil {
		s.sub.rearm(seenTotal)
	}
}

// DropStats reports how many published events a subscription could not receive.
type DropStats struct {
	// Total is the number of lost events across all topics.
	Total int64
	// ByTopic counts lost events per actual published topic.
	ByTopic map[string]int64
	// FirstTopic is the topic of the first event that was lost.
	FirstTopic string
	// FirstAt is when the first event was lost.
	FirstAt time.Time
	// LastAt is when the most recent event was lost.
	LastAt time.Time
}

// SubscriberStats describes one live subscription and its drop ledger.
type SubscriberStats struct {
	ID        int64
	Kind      string
	Topics    []string
	Capacity  int
	CreatedAt time.Time
	DropStats
}

// HubStats is the drop ledger of a hub and all of its live subscriptions.
type HubStats struct {
	// Dropped is the cumulative number of lost events, even after subscriptions ended.
	Dropped int64
	// ByTopic is the cumulative per-topic drop count.
	ByTopic map[string]int64
	// Subscribers lists the currently connected subscriptions.
	Subscribers []SubscriberStats
}

// subscription holds the channel, lifecycle state, and drop ledger of one subscriber.
type subscription struct {
	id       int64
	kind     string
	topics   []string
	patterns [][]string
	capacity int
	created  time.Time

	ch chan Message

	// sendMu covers channel sends versus close. Publishers hold its read lock for the whole
	// non-blocking send, so close can never shut a channel while a send is in flight.
	sendMu sync.RWMutex
	closed bool

	// statMu guards the drop ledger and the lag signal channel.
	statMu     sync.Mutex
	total      int64
	byTopic    map[string]int64
	firstTopic string
	firstAt    time.Time
	lastAt     time.Time
	lag        chan struct{}

	hub *Hub
}

// matches reports whether one of the subscription patterns matches the published topic words.
func (s *subscription) matches(words []string) bool {
	for _, pattern := range s.patterns {
		if matchPattern(pattern, words) {
			return true
		}
	}

	return false
}

// matchPattern matches topic segments one by one; a wildcard segment matches exactly one word.
func matchPattern(pattern, words []string) bool {
	if len(pattern) != len(words) {
		return false
	}

	for i, segment := range pattern {
		if segment != wildcard && segment != words[i] {
			return false
		}
	}

	return true
}

// set delivers one message without blocking the publisher. When the buffer is full the event is
// dropped exactly once and recorded in the subscription and hub ledgers.
func (s *subscription) set(m Message) {
	s.sendMu.RLock()
	defer s.sendMu.RUnlock()

	if s.closed {
		return
	}

	select {
	case s.ch <- m:
	default:
		s.recordDrop(m.Name)
	}
}

// recordDrop accounts for one lost event in both ledgers and signals the overflow to waiters.
func (s *subscription) recordDrop(topic string) {
	now := time.Now()

	s.statMu.Lock()

	first := s.total == 0
	s.total++

	if s.byTopic == nil {
		s.byTopic = make(map[string]int64)
	}

	s.byTopic[topic]++

	if first {
		s.firstTopic = topic
		s.firstAt = now
	}

	s.lastAt = now

	select {
	case <-s.lag:
		// The pending signal has not been acknowledged yet.
	default:
		close(s.lag)
	}

	h := s.hub
	kind := s.kind
	capacity := s.capacity

	s.statMu.Unlock()

	h.addDrop(topic)

	if first {
		SystemLog.Warnf("event: %s subscription buffer full (capacity %d); events will be dropped until the subscriber catches up, first lost topic %s",
			kind, capacity, clean.LogQuote(topic))
	}
}

// snapshot returns a copied point-in-time view of the subscription drop ledger.
func (s *subscription) snapshot() DropStats {
	s.statMu.Lock()
	defer s.statMu.Unlock()

	byTopic := make(map[string]int64, len(s.byTopic))

	for topic, count := range s.byTopic {
		byTopic[topic] = count
	}

	return DropStats{
		Total:      s.total,
		ByTopic:    byTopic,
		FirstTopic: s.firstTopic,
		FirstAt:    s.firstAt,
		LastAt:     s.lastAt,
	}
}

// lagChannel returns the currently armed lag notification channel.
func (s *subscription) lagChannel() <-chan struct{} {
	s.statMu.Lock()
	defer s.statMu.Unlock()

	return s.lag
}

// rearm installs a fresh lag notification channel that is already closed if more drops happened
// after the caller observed seenTotal.
func (s *subscription) rearm(seenTotal int64) {
	s.statMu.Lock()
	defer s.statMu.Unlock()

	ch := make(chan struct{})

	if s.total != seenTotal {
		close(ch)
	}

	s.lag = ch
}

// close marks the subscription closed and shuts its channel after in-flight sends finished.
func (s *subscription) close() {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// Subscribe creates a non-blocking subscription with the capacity of the default kind.
func Subscribe(topics ...string) Subscription {
	s, err := SharedHub().SubscribeKind(KindDefault, topics...)
	if err != nil {
		panic(err)
	}

	return s
}

// SubscribeKind creates a subscription on the shared hub using the capacity configured for kind.
func SubscribeKind(kind string, topics ...string) (Subscription, error) {
	return SharedHub().SubscribeKind(kind, topics...)
}

// SubscribeCap creates a subscription on the shared hub with an explicit channel capacity.
func SubscribeCap(capacity int, topics ...string) (Subscription, error) {
	return SharedHub().SubscribeCap(capacity, topics...)
}

// Unsubscribe deletes the subscription and closes its channel.
func Unsubscribe(s Subscription) {
	SharedHub().unsubscribe(s)
}

// Stats returns the drop ledger of the shared hub.
func Stats() HubStats {
	return SharedHub().Stats()
}

// Topic splits the topic name into the channel and event names.
func Topic(topic string) (ch, ev string) {
	ch, ev, _ = strings.Cut(topic, TopicSep)
	return ch, ev
}
