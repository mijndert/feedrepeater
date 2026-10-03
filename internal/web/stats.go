package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"feedrepeater.com/internal/destination"
)

// statsTTL is how long a computed answer is reused.
//
// /stats is public and every field behind it is a count(*), which SQLite walks
// row by row — so on a database with a few million deliveries it is the single
// most expensive read the service performs, reachable by anyone, with no
// session. It is computed at most once a minute and handed out from memory in
// between. A stats page a minute stale is not a stats page anyone notices.
const statsTTL = time.Minute

// statsMaxAge is when a cached answer stops being worth serving at all. Past
// the TTL a request gets the stale bytes and triggers a refresh behind it;
// past this it waits for a fresh one, because numbers this old are wrong rather
// than merely late.
const statsMaxAge = 15 * time.Minute

type statsFeeds struct {
	Total   int `json:"total"`
	Active  int `json:"active"`
	Stopped int `json:"stopped"`
	// Subscriptions is how many accounts follow a feed, counted across all of
	// them. It exceeds Total by however much sharing is saving: the difference
	// is fetches that used to be made and are not.
	Subscriptions int `json:"subscriptions"`
}

type statsDestinations struct {
	Total int `json:"total"`
	// ByKind carries every supported kind, including the ones on zero, so the
	// shape does not change as destinations come and go.
	ByKind map[string]int `json:"by_kind"`
}

type statsDeliveries struct {
	Total   int `json:"total"`
	Sent    int `json:"sent"`
	Pending int `json:"pending"`
	Failed  int `json:"failed"`
}

type statsService struct {
	PollInterval string `json:"poll_interval"`
	// AcceptingSignups is published rather than the raw FR_MAX_ACCOUNTS cap: the
	// useful fact is whether the door is open, not how wide it is.
	AcceptingSignups bool   `json:"accepting_signups"`
	StartedAt        string `json:"started_at"`
	UptimeSeconds    int64  `json:"uptime_seconds"`
}

type statsResponse struct {
	Users        int               `json:"users"`
	Instances    int               `json:"instances"`
	Feeds        statsFeeds        `json:"feeds"`
	Items        int               `json:"items"`
	Destinations statsDestinations `json:"destinations"`
	Deliveries   statsDeliveries   `json:"deliveries"`
	Service      statsService      `json:"service"`
}

// handleStats publishes aggregate counts for the whole service.
//
// This is served without a session, so everything in it is a count: no handle,
// feed URL, instance host, destination label or error string appears. Feed and
// destination totals describe the service; per-account detail stays behind
// requireUser, where the dashboard already shows it to the one person entitled
// to see it.
//
// Instance and account totals are the deliberate judgement call. They are what a
// status page exists to answer, and neither tells an attacker anything they can
// act on — unlike the raw signup cap, which is why only the open/closed state of
// it is published.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	body, err := s.stats(r.Context())
	if err != nil {
		s.log.Error("stats", "error", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	// Matches the memo window, so a cache in front of this asks no more often
	// than the numbers actually change.
	h.Set("Cache-Control", "public, max-age=60")
	w.Write(body)
}

// stats returns the encoded response, recomputing it at most once per statsTTL.
//
// Past the TTL the caller is handed the bytes that are already there and a
// refresh is started behind them. Recomputing in the request meant that whoever
// happened to arrive first after expiry paid for the whole table scan, and
// anyone arriving during it waited too — so the endpoint's cost landed on
// visitors at random, and hardest exactly when the database was largest. Now
// nobody waits on it unless the numbers have gone properly stale.
//
// The encoded bytes are cached rather than the struct, because every caller
// wants the same bytes and encoding them once is the cheaper half.
func (s *Server) stats(ctx context.Context) ([]byte, error) {
	s.statsMu.Lock()
	age := time.Since(s.statsAt)
	fresh := s.statsBody != nil && age < statsTTL
	servable := s.statsBody != nil && age < statsMaxAge

	if fresh {
		body := s.statsBody
		s.statsMu.Unlock()
		return body, nil
	}

	// One compute at a time, whatever the state of the cache.
	//
	// Guarding only the stale-but-servable path left the expensive case — cold
	// cache, or nothing asked for a quarter of an hour — completely open: two
	// hundred requests arriving after a restart ran two hundred concurrent
	// eleven-way count(*) scans, each holding a reader connection from a pool
	// sixteen wide. That is precisely the cost this cache exists to remove,
	// relocated to the worst moment to pay it, by anyone, unauthenticated.
	if s.statsRefresh {
		if servable {
			body := s.statsBody
			s.statsMu.Unlock()
			return body, nil
		}
		// Nothing worth serving, so wait for whoever is already computing.
		wait := s.statsWait
		s.statsMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.statsMu.Lock()
		body := s.statsBody
		s.statsMu.Unlock()
		if body == nil {
			return nil, errors.New("stats unavailable")
		}
		return body, nil
	}

	s.statsRefresh = true
	s.statsWait = make(chan struct{})
	if servable {
		// Serve what is there and refresh behind them. Detached from the
		// request: this visitor is not waiting for it, and cancelling when they
		// navigate away would mean it never completes for anyone.
		body := s.statsBody
		s.statsMu.Unlock()
		go s.refreshStats(context.WithoutCancel(ctx))
		return body, nil
	}
	s.statsMu.Unlock()
	// This caller does the compute itself, so it holds the claim and must
	// release it — including on error, or nothing would ever compute again.
	defer s.finishRefresh()
	return s.computeStats(ctx)
}

// finishRefresh releases the single-compute claim and wakes anyone waiting.
func (s *Server) finishRefresh() {
	s.statsMu.Lock()
	s.statsRefresh = false
	if s.statsWait != nil {
		close(s.statsWait)
		s.statsWait = nil
	}
	s.statsMu.Unlock()
}

func (s *Server) refreshStats(ctx context.Context) {
	defer s.finishRefresh()
	if _, err := s.computeStats(ctx); err != nil {
		s.log.Error("refresh stats", "error", err)
	}
}

// computeStats does the scan and stores the result. Its caller holds the
// single-compute claim and is responsible for releasing it via finishRefresh.
func (s *Server) computeStats(ctx context.Context) ([]byte, error) {
	now := time.Now()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	st, err := s.store.Stats(ctx)
	if err != nil {
		return nil, err
	}

	byKind := make(map[string]int, len(destination.Kinds))
	for _, k := range destination.Kinds {
		byKind[k.Name] = st.DestinationsByKind[k.Name]
	}

	res := statsResponse{
		Users:     st.Users,
		Instances: st.Instances,
		Feeds: statsFeeds{
			Total:         st.Feeds,
			Active:        st.FeedsActive,
			Stopped:       st.Feeds - st.FeedsActive,
			Subscriptions: st.Subscriptions,
		},
		Items: st.Items,
		Destinations: statsDestinations{
			Total:  st.Destinations,
			ByKind: byKind,
		},
		Deliveries: statsDeliveries{
			Total:   st.Deliveries,
			Sent:    st.DeliveriesSent,
			Pending: st.DeliveriesPending,
			Failed:  st.DeliveriesFailed,
		},
		Service: statsService{
			PollInterval: s.cfg.PollInterval.String(),
			// A zero cap means no cap, the same reading signupRefusal uses.
			AcceptingSignups: s.cfg.MaxAccounts == 0 || st.Users < s.cfg.MaxAccounts,
			StartedAt:        s.startedAt.UTC().Format(time.RFC3339),
			UptimeSeconds:    int64(now.Sub(s.startedAt).Seconds()),
		},
	}

	// Indented because the usual way anyone reads this is by opening it.
	body, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')

	s.statsMu.Lock()
	// Never move the timestamp backwards. Two overlapping computes each stamp
	// the time they started, so a slow early one landing after a fast later one
	// would re-arm expiry and undo the newer answer.
	if now.After(s.statsAt) {
		s.statsBody, s.statsAt = body, now
	}
	s.statsMu.Unlock()
	return body, nil
}
