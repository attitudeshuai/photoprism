/*
Package batchresult defines the shared per-item outcome structure returned by
batch endpoints so photos, albums, labels, and batch edit operations expose the
same success, partial-success, and failure semantics to their clients.

Copyright (c) 2018 - 2026 PhotoPrism UG. All rights reserved.

	This program is free software: you can redistribute it and/or modify
	it under Version 3 of the GNU Affero General Public License (the "AGPL"):
	<https://docs.photoprism.app/license/agpl>

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	The AGPL is supplemented by our Trademark and Brand Guidelines,
	which describe how our Brand Assets may be used:
	<https://www.photoprism.app/trademark/>

Feel free to send an email to hello@photoprism.app if you have questions,
want to support our work, or just want to say hello.

Additional information can be found in our Developer Guide:
<https://docs.photoprism.app/developer-guide/>
*/
package batchresult

import "errors"

// Batch result status values. They distinguish requests in which every selected
// item was applied from partially and completely unsuccessful ones, even when
// the HTTP status code stays the same for backwards compatibility.
const (
	StatusSuccess = "success"
	StatusPartial = "partial"
	StatusFailed  = "failed"
)

// Standard, stable failure reasons shared by batch endpoints. Endpoint-specific
// causes may use any other error whose message is safe to expose to clients.
var (
	// ErrNotFound indicates that a selected item does not exist or is not visible to the session.
	ErrNotFound = errors.New("item not found")
	// ErrStopped indicates that an item was not processed because fail-fast mode stopped the batch.
	ErrStopped = errors.New("item not processed: batch stopped after an earlier error")
	// ErrConflict indicates that a concurrent operation changed the item after the batch wrote it.
	ErrConflict = errors.New("item changed by a concurrent operation")
)

// Item describes the outcome of a single selected item.
type Item struct {
	// ID is the UID of the selected item.
	ID string `json:"id"`
	// OK reports whether the requested operation took effect for this item.
	OK bool `json:"ok"`
	// Error carries the failure reason; it is empty for successful items.
	Error string `json:"error,omitempty"`
}

// Result summarizes the outcome of a batch operation. It is embedded into the
// JSON responses of all batch endpoints alongside their usual message fields.
type Result struct {
	// Status is "success", "partial", or "failed".
	Status string `json:"status,omitempty"`
	// SuccessCount is the number of items the operation was applied to.
	SuccessCount int `json:"successCount,omitempty"`
	// FailCount is the number of items that could not be processed as requested.
	FailCount int `json:"failCount,omitempty"`
	// Stopped is true when fail-fast mode ended the batch before all items were processed.
	Stopped bool `json:"stopped,omitempty"`
	// Items holds the per-item outcomes in selection order. Responses to "all"
	// selections only contain failed items to keep the payload bounded.
	Items []Item `json:"items,omitempty"`
}

// itemState tracks how a pre-registered item was resolved.
type itemState uint8

const (
	statePending itemState = iota
	stateSuccess
	stateFailed
)

// Builder accumulates per-item outcomes in selection order and is safe for
// sequential use within a single request handler.
type Builder struct {
	ids        []string
	state      map[string]itemState
	reasons    map[string]string
	implicitOK int
	stopped    bool
}

// NewBuilder returns a Builder pre-seeded with the expected item IDs in their
// first-seen order. Duplicate IDs are recorded only once.
func NewBuilder(expected ...string) *Builder {
	b := &Builder{
		state:   make(map[string]itemState, len(expected)),
		reasons: make(map[string]string),
	}

	for _, id := range expected {
		if id == "" {
			continue
		}

		if _, registered := b.state[id]; registered {
			continue
		}

		b.state[id] = statePending
		b.ids = append(b.ids, id)
	}

	return b
}

// IDs returns the unique pre-registered item IDs in first-seen order.
func (b *Builder) IDs() []string {
	return b.ids
}

// Success records that the operation was applied to the given item.
func (b *Builder) Success(id string) {
	if id == "" {
		return
	}

	if _, registered := b.state[id]; !registered {
		b.state[id] = statePending
		b.ids = append(b.ids, id)
	}

	b.state[id] = stateSuccess
	delete(b.reasons, id)
}

// Failure records that the operation could not be applied to the given item and
// stores the given reason.
func (b *Builder) Failure(id string, reason error) {
	if id == "" {
		return
	}

	if _, registered := b.state[id]; !registered {
		b.state[id] = statePending
		b.ids = append(b.ids, id)
	}

	b.state[id] = stateFailed

	if reason != nil {
		b.reasons[id] = reason.Error()
	}
}

// AddSuccess counts successful items without adding per-item entries, e.g. for
// an unbounded "all" selection where listing every ID would be impractical.
func (b *Builder) AddSuccess(n int) {
	if n > 0 {
		b.implicitOK += n
	}
}

// Stop marks all items that were not processed yet as failed with ErrStopped
// and records that fail-fast mode ended the batch early.
func (b *Builder) Stop() {
	b.stopped = true
	b.FailPending(ErrStopped)
}

// FailPending marks every item without an explicit outcome with the given reason.
func (b *Builder) FailPending(reason error) {
	if reason == nil {
		reason = ErrNotFound
	}

	for _, id := range b.ids {
		if b.state[id] == statePending {
			b.state[id] = stateFailed
			b.reasons[id] = reason.Error()
		}
	}
}

// Succeeded returns the IDs of all successful items in first-seen order, for
// emitting change events only for items that were actually modified.
func (b *Builder) Succeeded() []string {
	result := make([]string, 0, len(b.ids))

	for _, id := range b.ids {
		if b.state[id] == stateSuccess {
			result = append(result, id)
		}
	}

	return result
}

// HasFailures reports whether at least one item failed.
func (b *Builder) HasFailures() bool {
	for _, st := range b.state {
		if st == stateFailed {
			return true
		}
	}

	return false
}

// Result builds the immutable Result snapshot. Items still pending are treated
// as not found so an unreported item can never appear successful.
func (b *Builder) Result() *Result {
	b.FailPending(ErrNotFound)

	r := &Result{Stopped: b.stopped, SuccessCount: b.implicitOK}

	for _, id := range b.ids {
		switch b.state[id] {
		case stateSuccess:
			r.SuccessCount++
			r.Items = append(r.Items, Item{ID: id, OK: true})
		case stateFailed:
			r.FailCount++
			r.Items = append(r.Items, Item{ID: id, OK: false, Error: b.reasons[id]})
		}
	}

	switch {
	case r.FailCount == 0:
		r.Status = StatusSuccess
	case r.SuccessCount == 0:
		r.Status = StatusFailed
	default:
		r.Status = StatusPartial
	}

	return r
}
