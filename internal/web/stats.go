package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"feedrepeater.com/internal/destination"
)

// statsTTL is how long a computed answer is reused.
//
// /stats is public and every field behind it is a count(*), which SQLite walks
// row by row. With a single-connection pool that makes an uncached endpoint a
// cheap way to serialise every other request behind it, so the numbers are
// computed at most once a minute and handed out from memory in between. A stats
// page a minute stale is not a stats page anyone notices.
const statsTTL = time.Minute

type statsFeeds struct {
	Total  int `json:"total"`
	Active int `json:"active"`
	Paused int `json:"paused"`
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

// stats returns the encoded response, recomputing it only once per statsTTL.
//
// The encoded bytes are cached rather than the struct, because every caller
// wants the same bytes and encoding them once is the cheaper half.
func (s *Server) stats(ctx context.Context) ([]byte, error) {
	now := time.Now()

	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.statsBody != nil && now.Sub(s.statsAt) < statsTTL {
		return s.statsBody, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
			Total:  st.Feeds,
			Active: st.FeedsActive,
			Paused: st.Feeds - st.FeedsActive,
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
			PollInterval: s.cfg.MinPollInterval.String(),
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

	s.statsBody, s.statsAt = body, now
	return body, nil
}
