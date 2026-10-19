package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/pkg/rnd"
)

func TestWsLagPayload(t *testing.T) {
	data := wsLagPayload(event.DropStats{
		Total:      7,
		FirstTopic: "photos.updated",
		ByTopic:    map[string]int64{"photos.updated": 4, "count.photos": 3},
	})

	assert.Equal(t, true, data["lag"])
	assert.Equal(t, int64(7), data["dropped"])
	assert.Equal(t, "photos.updated", data["firstTopic"])
	assert.Equal(t, map[string]int64{"photos.updated": 4, "count.photos": 3}, data["topics"])
}

// waitForWebsocketSubscriber polls the shared hub until the connection's subscription is registered.
func waitForWebsocketSubscriber(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		for _, s := range event.Stats().Subscribers {
			if s.Kind == event.KindWebsocket {
				return
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("websocket subscription was not registered")
}

func TestWebSocketSendsLagSignalOnOverflow(t *testing.T) {
	// A capacity of one guarantees overflow when several events are published at once.
	prev, hadPrev := event.Capacity(event.KindWebsocket)
	require.NoError(t, event.SetCapacity(event.KindWebsocket, 1))

	t.Cleanup(func() {
		if hadPrev {
			_ = event.SetCapacity(event.KindWebsocket, prev)
		}
	})

	upgrader := websocket.Upgrader{}
	connId := rnd.UUID()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)

		var writeMutex sync.Mutex
		go wsWriter(ws, &writeMutex, connId)
	}))

	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)

	defer client.Close()

	waitForWebsocketSubscriber(t)

	for i := 0; i < 5; i++ {
		event.Publish("index.lag-test", event.Data{"seq": i})
	}

	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))

	var lag event.Data

	for {
		_, message, readErr := client.ReadMessage()
		require.NoError(t, readErr, "waiting for the lag signal")

		var received struct {
			Event string     `json:"event"`
			Data  event.Data `json:"data"`
		}

		require.NoError(t, json.Unmarshal(message, &received))

		if received.Event == wsLagEvent {
			lag = received.Data
			break
		}
	}

	assert.Equal(t, true, lag["lag"])
	assert.GreaterOrEqual(t, lag["dropped"].(float64), float64(1))
	assert.Equal(t, "index.lag-test", lag["firstTopic"])

	topics, ok := lag["topics"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, topics, "index.lag-test")
}
