## PhotoPrism — Event System

**Last Updated:** October 15, 2026

### Overview

`internal/event` provides a lightweight pub/sub hub for in-process notifications. It underpins logging hooks, UI notifications, and domain events (entities created/updated/deleted/archived/restored). Subscriptions are non-blocking: a subscriber whose buffer is full never stalls a publisher. Overflow is not silent — every lost event is counted per topic per subscription, cumulative hub-wide counters are kept for operators, and subscriptions expose a lag signal so forwarders such as the websocket writer can tell their clients to resynchronize.

### Usage

Publish a custom event:
```go
event.Publish("photos.updated", event.Data{"ids": []string{"p1", "p2"}})
```

Publish localized notifications:
```go
event.SuccessMsg(i18n.MsgIndexingCompletedIn, elapsed)
event.Warn("low disk space")
```

The `*Msg` helpers (`SuccessMsg`/`ErrorMsg`/`InfoMsg`/`WarnMsg`) publish a structured payload
`Data{"message", "messageId", "messageParams"}`: `message` is the server-rendered string in the
instance locale, `messageId` is the untranslated source string (`i18n.Source(id)`), and
`messageParams` are the substitution values. The Web UI renders the notification from `messageId` +
`messageParams` in each user's current UI language; `message` is a fallback. The accompanying log
line is written with `i18n.Lower(id, params...)`, so server logs stay English in any locale. Use
`PublishSuccessMsg` to publish without a log line when the caller logs a more specific one. The plain
`Success`/`Error`/`Info`/`Warn` string forms publish only `message` and are **not** localized —
reserve them for already-translated or non-user-facing text.

Subscribe to topics:
```go
sub := event.Subscribe("photos.*")
defer event.Unsubscribe(sub)
for msg := range sub.Receiver {
    fmt.Printf("topic=%s payload=%v\n", msg.Name, msg.Fields)
}
```

Topic patterns match whole dot-separated segments; `*` matches exactly one segment, so `photos.*` matches `photos.updated` but not `photos.updated.extra`.

### Subscription Capacity & Overflow

Each subscription owns a buffered, non-blocking channel. Capacities are configured per subscription kind:

```go
// Configure a kind at startup; values outside event.MinCap..event.MaxCap are rejected.
if err := event.SetCapacity(event.KindWebsocket, 256); err != nil {
    return err
}

// Subscribe with the capacity of a configured kind.
sub, err := event.SubscribeKind(event.KindWebsocket, "photos.*")
```

`event.Subscribe` uses the `default` kind (capacity 100); `SubscribeCap` sets an explicit capacity and returns `ErrInvalidCapacity` for invalid values. Unknown kinds fail with `ErrUnknownKind` until `SetCapacity` registers them.

When a published event does not fit a subscriber's buffer, it is dropped exactly once and recorded:
- Per subscription: `sub.DroppedTotal()`, `sub.DroppedByTopic()`, and `sub.DropStats()` report the count, the per-topic breakdown, and the topic and time of the first and most recent loss.
- Lag signal: `sub.LagNotify()` closes right after the first loss since `sub.AckLag(total)`; forwarders select on it and send a resynchronization notice instead of polling.
- Hub-wide: `event.Stats()` returns cumulative per-topic counters that survive unsubscribe plus snapshots of all live subscriptions. Operators read them through the `photoprism_events_dropped{topic="..."}` and `photoprism_events_subscriptions` metrics on `/api/v1/metrics`, and the first loss of each subscription is written to the system log.

Without overflow, message order, content, and delivery timing are unchanged; publishing helpers never block on subscribers.

Log hook (used by default logger):
```go
hook := event.NewHook(event.SharedHub())
log.AddHook(hook)
```

The hook publishes each entry as `log.<level>` for the live log and the errors view. In the published message, every run of 32 or more hex digits is masked except for its first three digits, e.g. `2ca***` (`clean.MaskHashes`), while the console output keeps the full text. `notify.*` messages are masked the same way; `event.Audit*` is not.

Entity events (content-channel payloads carry only identity strings — UIDs/slugs — never entity bodies):
```go
event.EntitiesUpdated("photos", []string{photo.PhotoUID})
event.EntitiesDeleted("labels", []string{label.LabelUID})
```

Background workers that change many entities batch their events, so a fast run sends one event per 50 entities while a slow one still notifies about once per entity. A batch publishes when 50 identities are pending or 2 seconds have passed since its last publish:
```go
batch := event.NewEntityBatch("photos", event.EntityUpdated)
defer batch.Flush()

for _, photo := range photos {
    batch.FlushDue()
    // ... save the photo ...
    batch.Add(photo.PhotoUID)
}

batch.Flush()
```

### Package Layout (Code Map)

- Hub aliases & helpers: `hub.go`, `format.go`, `time.go`
- Logging hook: `log.go`
- Publish helpers: `publish.go`, `publish_entities.go`, `entity_batch.go`
- Tests: package-level tests alongside sources

### Related Packages

- `internal/photoprism` — core indexing/import flows that emit events.
- `internal/api` — websocket writer that forwards events and sends the `websocket.lag` signal; metrics endpoint exposing the drop ledger.
- `internal/ai/vision` & `internal/ffmpeg` — emit log events via the shared logger.

### Testing

- Lint: `golangci-lint run ./internal/event...`
- Unit tests: `go test ./internal/event/...` (lightweight)

### Notes

- Use `SharedHub()` for process-wide subscriptions; `NewHub()` when isolating tests.
- Topic separator is `.`; message separator for rendering is ` › `.
- Keep notifications human-readable; payloads should be small to avoid blocking subscribers.
