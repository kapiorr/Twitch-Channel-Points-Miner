package classes

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ? pubSubTestServer is a local stand-in for Twitch PubSub; serve runs once per connection.
func pubSubTestServer(t *testing.T, serve func(conn *websocket.Conn)) *PubSubClient {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		serve(conn)
	}))
	t.Cleanup(srv.Close)
	return &PubSubClient{logger: &stubPubSubLogger{}, endpoint: "ws" + strings.TrimPrefix(srv.URL, "http")}
}

func waitForConnection(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("connectAndListen did not return (hung)")
		return nil
	}
}

func readUntilClosed(conn *websocket.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func TestConnectAndListenReturnsWhenSubscriptionFails(t *testing.T) {
	// ? The server drops the connection at once, so writing the LISTEN messages fails.
	// ? Before the fix the deferred cleanup waited for a reader that never started.
	p := pubSubTestServer(t, func(conn *websocket.Conn) {})
	topics := make([]string, 5000)
	for i := range topics {
		topics[i] = fmt.Sprintf("video-playback-by-id.%d", i)
	}
	done := make(chan error, 1)
	go func() { done <- p.connectAndListen(1, topics, make(chan struct{})) }()
	if err := waitForConnection(t, done); err == nil {
		t.Fatalf("a failed subscription should be reported as an error")
	}
}

func TestConnectAndListenSubscribesAndHonoursReconnect(t *testing.T) {
	listened := make(chan map[string]interface{}, 1)
	p := pubSubTestServer(t, func(conn *websocket.Conn) {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		listened <- msg
		_ = conn.WriteJSON(map[string]string{"type": "PONG"})
		_ = conn.WriteJSON(map[string]string{"type": "RECONNECT"})
		readUntilClosed(conn)
	})
	done := make(chan error, 1)
	go func() { done <- p.connectAndListen(1, []string{"video-playback-by-id.123"}, make(chan struct{})) }()
	if err := waitForConnection(t, done); !errors.Is(err, ErrPubSubReconnectRequested) {
		t.Fatalf("got %v, want a reconnect request", err)
	}
	msg := <-listened
	data, _ := msg["data"].(map[string]interface{})
	topics, _ := data["topics"].([]interface{})
	if msg["type"] != "LISTEN" || len(topics) != 1 || topics[0] != "video-playback-by-id.123" {
		t.Fatalf("unexpected subscription message %v", msg)
	}
	if _, hasToken := data["auth_token"]; hasToken {
		t.Fatalf("a public topic should not carry the auth token")
	}
}

func TestConnectAndListenStopsCleanly(t *testing.T) {
	p := pubSubTestServer(t, readUntilClosed)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- p.connectAndListen(1, nil, stop) }()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	if err := waitForConnection(t, done); err != nil {
		t.Fatalf("stopping should not be an error, got %v", err)
	}
}

func TestConnectAndListenReportsLostConnection(t *testing.T) {
	p := pubSubTestServer(t, func(conn *websocket.Conn) {})
	done := make(chan error, 1)
	go func() { done <- p.connectAndListen(1, nil, make(chan struct{})) }()
	if err := waitForConnection(t, done); err == nil || errors.Is(err, ErrPubSubReconnectRequested) {
		t.Fatalf("got %v, want a read error", err)
	}
}
