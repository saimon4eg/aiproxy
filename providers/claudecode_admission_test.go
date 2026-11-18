package providers

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The relay admits exactly one generation per request: a CLI that retries on its
// own must not draw the subscription twice.
func TestAdmissionAdmitsOnlyOneGeneration(t *testing.T) {
	hits := 0
	// The strict upstream rejects a wrong path, so a doubled /v1/messages cannot
	// slip through as a "successful" second call again.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream got path %q, want /v1/messages", r.URL.Path)
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	// The relay gets the provider's BASE url; it appends the CLI's own path. This
	// test used to pass "<base>/v1/messages" here, which locked in the doubled
	// /v1/messages/v1/messages path — the assertion below now pins the real one.
	relay, err := newAdmission(upstream.URL, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	var statuses []int
	for i := 0; i < 3; i++ {
		resp, err := client.Post(relay.baseURL()+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		statuses = append(statuses, resp.StatusCode)
		resp.Body.Close()
	}

	if hits != 1 {
		t.Fatalf("upstream reached %d times, want 1 (statuses %v)", hits, statuses)
	}
	if statuses[0] != http.StatusOK {
		t.Fatalf("first attempt should pass, got %d", statuses[0])
	}
	for _, status := range statuses[1:] {
		if status != http.StatusTooManyRequests {
			t.Fatalf("repeat attempt should be refused locally, got %d", status)
		}
	}
	if _, denied := relay.stats(); denied != 2 {
		t.Fatalf("denied counter %d, want 2", denied)
	}
}

// A probe that is not a generation must not consume the admission slot.
func TestAdmissionIgnoresProbes(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	relay, err := newAdmission(upstream.URL, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	probe, err := client.Get(relay.baseURL() + "/api/hello")
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("probe failed: %d", probe.StatusCode)
	}

	resp, err := client.Post(relay.baseURL()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("generation after a probe should pass, got %d", resp.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("upstream reached %d times, want 1", hits)
	}
}

// The capture keeps the answer even when native reports something else.
func TestAdmissionCapturesStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	relay, err := newAdmission(upstream.URL, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	resp, err := client.Post(relay.baseURL()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	captured := relay.wait(5 * time.Second)
	if captured == nil {
		t.Fatal("no capture")
	}
	if !captured.Complete {
		t.Fatal("stream ending in message_stop must count as complete")
	}
	if !strings.Contains(string(captured.Body), "hi") {
		t.Fatalf("captured body lost the text: %s", captured.Body)
	}
}

// A probe racing a generation must not steal the generation's admission slot.
// The old code released the slot after forwarding, so a probe in flight reset
// `used` and the generation downstream was refused with 429.
func TestAdmissionProbeDoesNotStealSlot(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hello" {
			<-release // hold the probe open while the generation arrives
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	relay, err := newAdmission(upstream.URL, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	probeDone := make(chan int, 1)
	go func() {
		resp, err := client.Get(relay.baseURL() + "/api/hello")
		if err != nil {
			probeDone <- -1
			return
		}
		resp.Body.Close()
		probeDone <- resp.StatusCode
	}()

	// Give the probe time to be in flight before the generation starts.
	time.Sleep(80 * time.Millisecond)

	resp, err := client.Post(relay.baseURL()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("generation racing a probe got %d, want 200", resp.StatusCode)
	}
	close(release)
	if status := <-probeDone; status != http.StatusOK {
		t.Fatalf("probe got %d", status)
	}
}

// The CLI calls /v1/messages?beta=true. The relay must forward the query, or the
// upstream takes a different code path than the real client asked for.
func TestAdmissionForwardsQuery(t *testing.T) {
	var gotQuery, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	relay, err := newAdmission(upstream.URL, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	resp, err := client.Post(relay.baseURL()+"/v1/messages?beta=true", "application/json",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotPath != "/v1/messages" {
		t.Fatalf("path %q, want /v1/messages", gotPath)
	}
	if gotQuery != "beta=true" {
		t.Fatalf("query %q, want beta=true", gotQuery)
	}
}

// A gzipped upstream must reach the CLI as plain bytes. The relay forwards no
// Accept-Encoding and lets Go negotiate, so the body arrives decompressed and
// Content-Encoding never reaches the client.
func TestAdmissionDecodesGzippedUpstream(t *testing.T) {
	const payload = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"plain"}]}`

	var sawAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte(payload))
		zw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		w.Write(buf.Bytes())
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	relay, err := newAdmission(upstream.URL, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	// The CLI advertises exactly this on a real run.
	req, err := http.NewRequest(http.MethodPost, relay.baseURL()+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("client was told the body is %q-encoded, but it is plain", got)
	}
	if string(body) != payload {
		t.Fatalf("body is not the decoded payload:\n got %q\nwant %q", body, payload)
	}
	// The client's list must not be forwarded: Go has to negotiate for itself.
	if sawAcceptEncoding == "gzip, deflate, br, zstd" {
		t.Fatalf("the client's Accept-Encoding was forwarded verbatim: %q", sawAcceptEncoding)
	}
}
