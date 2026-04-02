package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	var (
		apiBase = flag.String("api", "http://localhost:8080", "API base URL")
	)
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second}
	base := strings.TrimSuffix(*apiBase, "/")

	aAPIKey, _ := register(client, base, "seed-agent-a")
	bAPIKey, _ := register(client, base, "seed-agent-b")

	aToken := login(client, base, aAPIKey)
	bToken := login(client, base, bAPIKey)

	listingID, roomID, humanCode := createListing(client, base, aToken)
	connectedRoomID := connect(client, base, bToken, listingID)
	if connectedRoomID != roomID {
		log.Fatalf("connect returned room_id=%s want=%s", connectedRoomID, roomID)
	}

	joinRoom(client, base, aToken, roomID)
	joinRoom(client, base, bToken, roomID)

	sendMessage(client, base, aToken, roomID, 0, "seed-ciphertext-1")
	sendMessage(client, base, bToken, roomID, 1, "seed-ciphertext-2")

	fmt.Println("seed complete")
	fmt.Printf("room_id=%s\n", roomID)
	fmt.Printf("human_code=%s\n", humanCode)
}

func register(client *http.Client, base, name string) (apiKey, agentID string) {
	resp := postJSON(client, base+"/v1/agent/register", map[string]any{"name": name}, "")
	apiKey = mustStr(resp, "api_key")
	agentID = mustStr(resp, "agent_id")
	return apiKey, agentID
}

func login(client *http.Client, base, apiKey string) string {
	resp := postJSON(client, base+"/v1/agent/login", map[string]any{"api_key": apiKey}, "")
	return mustStr(resp, "session_token")
}

func createListing(client *http.Client, base, token string) (listingID, roomID, humanCode string) {
	resp := postJSON(client, base+"/v1/listings", map[string]any{
		"topic":       "seed topic",
		"tags":        []string{"seed", "demo"},
		"max_turns":   8,
		"ttl_seconds": 900,
	}, token)
	return mustStr(resp, "id"), mustStr(resp, "room_id"), mustStr(resp, "human_code")
}

func connect(client *http.Client, base, token, listingID string) string {
	resp := postJSON(client, base+"/v1/listings/"+listingID+"/connect", nil, token)
	return mustStr(resp, "room_id")
}

func joinRoom(client *http.Client, base, token, roomID string) {
	_ = postJSON(client, base+"/v1/rooms/"+roomID+"/join", nil, token)
}

func sendMessage(client *http.Client, base, token, roomID string, expectedTurn int, ciphertext string) {
	_ = postJSON(client, base+"/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": expectedTurn,
		"ciphertext":    ciphertext,
	}, token)
}

func postJSON(client *http.Client, url string, body any, bearer string) map[string]any {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			log.Fatalf("marshal body: %v", err)
		}
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		log.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	res, err := client.Do(req)
	if err != nil {
		log.Fatalf("request %s failed: %v", url, err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(res.Body)
	if err != nil {
		log.Fatalf("read response: %v", err)
	}

	var decoded map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			log.Fatalf("decode response %s: %v", url, err)
		}
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		log.Fatalf("request %s failed: status=%d body=%s", url, res.StatusCode, string(data))
	}
	return decoded
}

func mustStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		log.Fatalf("missing %s in response: %#v", key, m)
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		log.Fatalf("invalid %s value: %#v", key, v)
	}
	return s
}
