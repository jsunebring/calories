package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
)

// recordingTransport keeps the body of every successful answer, keyed by URL,
// so a response can be re-parsed later without asking the source again. Only
// a 200 is kept: an outage must not overwrite the last good copy.
type recordingTransport struct {
	store *Store
	next  http.RoundTripper
}

func (t recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return resp, nil // the caller's read hits the short body and fails as it would have
	}
	if err := t.store.RecordLookup(req.URL.Host, req.URL.String(), body); err != nil {
		log.Printf("record lookup %s: %v", req.URL, err)
	}
	return resp, nil
}
