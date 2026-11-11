package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestSendReachesServer exercises ONLY the notification path: build a
// synthetic message, Send() it, Wait() for drain (the fix), then poll
// the ntfy server's JSON feed for the message. This is the fast,
// GitHub-independent proof that the goroutine-exit bug is gone.
func TestSendReachesServer(t *testing.T) {
	base := os.Getenv("NTFY_BASE")
	topic := os.Getenv("NTFY_TOPIC")
	user := os.Getenv("NTFY_USER")
	pass := os.Getenv("NTFY_PASS")
	if base == "" || topic == "" {
		t.Skip("set NTFY_BASE / NTFY_TOPIC to run")
	}

	marker := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	n := newNotifier()
	n.URL, n.Topic, n.User, n.Pass = base, topic, user, pass

	title, body := BuildBatchNotification([]notifyEntry{
		{Repo: "amigoer/mq-studio", Tag: "v0.1.1", Packages: []string{"mq-studio-0.1.1-windows-amd64.exe"}},
	})
	body = marker + "\n" + body

	n.Send(title, body)
	if !n.Wait(10 * time.Second) {
		t.Fatalf("Wait() timed out — notification goroutine did not drain")
	}
	fmt.Println("send+wait returned; verifying server side...")

	// Poll the ntfy JSON feed for our marker.
	feed := fmt.Sprintf("%s/%s/json?poll=1&since=1m", base, topic)
	req, _ := http.NewRequest(http.MethodGet, feed, nil)
	if user != "" && pass != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("poll feed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if !bytes.Contains(raw, []byte(marker)) {
		t.Fatalf("marker %s NOT found in feed; server got:\n%s", marker, raw)
	}

	// Also decode to show the delivered message cleanly.
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m struct {
			Event   string `json:"event"`
			Message string `json:"message"`
			Title   string `json:"title"`
		}
		if json.Unmarshal(line, &m) == nil && m.Event == "message" {
			fmt.Printf("DELIVERED title=%q\n---\n%s\n---\n", m.Title, m.Message)
		}
	}
}
