package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const maxCDNCandidates = 16

type sourceState struct {
	RetryAfter   time.Time
	LastMedia    string
	LastProgress time.Time
	Error        string
}

type playlistResult struct {
	url      string
	playlist string
	err      error
}

// Called by the channel's sole refresh worker. Health is bounded by the
// current discovery response, and signed URL renewal can retry a new URL.
func (m *channelManager) setBKURLs(ch *channel, urls []string) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	states := make(map[string]sourceState)
	bounded := make([]string, 0, min(len(urls), maxCDNCandidates))
	for _, u := range urls {
		if _, exists := states[u]; exists {
			continue
		}
		state, known := ch.bkSources[u]
		if !known {
			// Renewing a signature may fix authorization, but must not make
			// a known frozen media timeline appear to advance again.
			for oldURL, old := range ch.bkSources {
				if segmentKey(oldURL, "") == segmentKey(u, "") && old.LastProgress.After(state.LastProgress) {
					state.LastMedia, state.LastProgress = old.LastMedia, old.LastProgress
				}
			}
		}
		states[u] = state
		bounded = append(bounded, u)
		if len(bounded) == maxCDNCandidates {
			break
		}
	}
	ch.bkURLs, ch.bkSources = bounded, states
}

func (m *channelManager) failBKSource(ch *channel, u string, err error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	state := ch.bkSources[u]
	state.RetryAfter = time.Now().Add(m.sourceCooldown)
	state.Error = truncateError(err, 120)
	ch.bkSources[u] = state
}

func (m *channelManager) acceptBKSource(ch *channel, result playlistResult) error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	now := time.Now()
	media := playlistMediaKey(result.playlist)
	state := ch.bkSources[result.url]
	// Check both this source's history and the last served playlist. A source
	// that returns 200 with frozen media must not win the candidate race.
	if (media == state.LastMedia && !state.LastProgress.IsZero() && now.Sub(state.LastProgress) >= m.maxPlaylistAge) ||
		(media == ch.lastMedia && !ch.lastProgress.IsZero() && now.Sub(ch.lastProgress) >= m.maxPlaylistAge) {
		return errors.New("upstream playlist stopped advancing")
	}
	if media != state.LastMedia || state.LastProgress.IsZero() {
		state.LastMedia, state.LastProgress = media, now
	}
	state.RetryAfter, state.Error = time.Time{}, ""
	ch.bkSources[result.url] = state
	ch.bkPreferred = result.url
	ch.bkPlaylist = result.playlist
	return nil
}

// Prefer the previous winner. If it is slow, hedge after a short delay, with
// at most two in-flight requests. Cancel and join all losers before returning.
func (m *channelManager) fetchBKCandidates(ctx context.Context, ch *channel) error {
	ch.mu.Lock()
	if ch.bkSources == nil {
		ch.bkSources = make(map[string]sourceState)
	}
	var urls []string
	var lastErr error
	for _, u := range ch.bkURLs {
		state := ch.bkSources[u]
		if time.Now().Before(state.RetryAfter) {
			lastErr = errors.New(state.Error)
			continue
		}
		urls = append(urls, u)
		if len(urls) == maxCDNCandidates {
			break
		}
	}
	preferred := ch.bkPreferred
	ch.mu.Unlock()
	if len(urls) == 0 {
		if lastErr != nil {
			return lastErr
		}
		return errors.New("no CDN candidates available")
	}
	rank := func(u string) int {
		if u == preferred {
			return 0
		}
		if segmentKey(u, "") == segmentKey(preferred, "") {
			return 1
		}
		return 2
	}
	sort.SliceStable(urls, func(i, j int) bool { return rank(urls[i]) < rank(urls[j]) })
	workCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	results := make(chan playlistResult, len(urls))
	next, active := 0, 0
	pending := make(map[string]bool)
	start := func() {
		u := urls[next]
		next++
		active++
		pending[u] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			requestCtx, stop := context.WithTimeout(workCtx, m.sourceTimeout)
			defer stop()
			playlist, err := fetchAbsolutePlaylist(requestCtx, m.client, u, 0, m.maxBandwidth)
			results <- playlistResult{u, playlist, err}
		}()
	}
	start()
	timer := time.NewTimer(m.hedgeDelay)
	defer timer.Stop()
	for active > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if active < 2 && next < len(urls) {
				start()
			}
		case result := <-results:
			active--
			delete(pending, result.url)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if result.err == nil {
				result.err = m.acceptBKSource(ch, result)
			}
			if result.err == nil {
				for u := range pending {
					m.failBKSource(ch, u, errors.New("slower than selected CDN"))
				}
				return nil
			}
			lastErr = result.err
			m.failBKSource(ch, result.url, result.err)
			if next < len(urls) {
				start()
			}
		}
	}
	return lastErr
}

func (m *channelManager) bkRefresh(ctx context.Context, ch *channel) error {
	// A successful CDN fetch proves the cached URL still works. Do not put
	// periodic discovery on the critical path merely because a TTL elapsed.
	err := m.fetchBKCandidates(ctx, ch)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	urls, discoveryErr := m.discoverBKURLs(ctx, ch)
	if discoveryErr != nil {
		return fmt.Errorf("%v; discovery: %w", err, discoveryErr)
	}
	m.setBKURLs(ch, urls)
	return m.fetchBKCandidates(ctx, ch)
}
