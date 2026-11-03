package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/tracker"
)

// timeNow is a seam for tests.
var timeNow = time.Now

const (
	eventPageMax     = 500
	eventPageDefault = 100
	sseWriteTimeout  = 30 * time.Second
)

func parseSeq(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, httpx.Invalid("after must be a non-negative integer seq")
	}
	return n, nil
}

// listEvents: GET .../events?after=<seq>&limit=. nextCursor is the last seq
// when the page is full; pass it back as `after`. When the page is not full
// but the feed was read past its last item (events the caller may not read,
// T20 #55), nextCursor is that high-water mark, so the next poll does not
// read those rows again. No nextCursor: the caller is at the end.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	after, err := parseSeq(r.URL.Query().Get("after"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	limit := eventPageDefault
	if l := r.URL.Query().Get("limit"); l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit < 1 || limit > eventPageMax {
			s.fail(w, r, httpx.Invalid("limit must be 1-%d", eventPageMax))
			return
		}
	}
	// Read before the page: see feedHighWater.
	hw, err := s.svc.LatestEventSeq(r.Context(), o.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	evs, err := s.svc.ListEvents(r.Context(), o, after, int32(limit))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page := httpx.Page[tracker.Event]{Items: evs}
	var next int64
	switch {
	case len(evs) == limit:
		next = evs[len(evs)-1].Seq
	case len(evs) > 0 && hw > evs[len(evs)-1].Seq, len(evs) == 0 && hw > after:
		next = hw
	}
	if next > 0 {
		c := strconv.FormatInt(next, 10)
		page.NextCursor = &c
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

// streamEvents: GET .../events/stream, Server-Sent Events.
//
// Resume point: Last-Event-ID (the browser sends it on reconnect), else
// ?after=, else "now" (only new events). Each event is written as
// `id: <seq>` / `event: <type>` / `data: <Event JSON>`. The handler wakes on
// LISTEN/NOTIFY through the broker, and also polls every SSEPoll so a lost
// notification only delays an event. A comment line every SSEHeartbeat keeps
// proxies from closing an idle stream.
//
// Team access (decisions-T20 #55, #56): the stream shows what GET /events
// shows the caller. At the start of every drain (a NOTIFY wake-up, a poll
// tick, the first read) it re-reads the token, the user, the membership, the
// role and the team access. When the token, the user or the membership is
// gone, the stream ends; a missing membership is never an empty set. When the
// role or the team access changed, it first sends `event: access` (data
// `{}`, no id, so Last-Event-ID does not move; the event is not stored), then
// drains with the new set.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	ctx := r.Context()

	resume := r.Header.Get("Last-Event-ID")
	if resume == "" {
		resume = r.URL.Query().Get("after")
	}
	var last int64
	if resume != "" {
		n, err := parseSeq(resume)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		last = n
	} else {
		n, err := s.svc.LatestEventSeq(ctx, o.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		last = n
	}

	// Subscribe before the first read, so nothing committed after the read
	// can be missed.
	wake, cancel := s.broker.Subscribe(o.ID)
	defer cancel()

	rc := http.NewResponseController(w)
	// A stream outlives the server ReadTimeout; without this the expired read
	// deadline would end the request.
	if err := rc.SetReadDeadline(time.Time{}); err != nil && err != http.ErrNotSupported {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx (CapRover): do not buffer
	w.WriteHeader(http.StatusOK)

	write := func(format string, args ...any) error {
		if err := rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil && err != http.ErrNotSupported {
			return err
		}
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return err
		}
		return rc.Flush()
	}
	view := *o // the caller's role and access as this stream last read them
	pageSize := eventPageDefault
	if s.eventStreamPage > 0 {
		pageSize = s.eventStreamPage
	}
	// recheck re-reads the credentials, the role and the access before every
	// page, so a removal takes effect within one page (#56).
	recheck := func() error {
		st, ok, err := s.svc.StillAuthorized(ctx, p, o.ID)
		if err != nil {
			return err
		}
		if !ok {
			// A stream must not outlive a revoked or expired token, a
			// deactivated user or a removed membership.
			_ = write(": credentials no longer valid\n\n")
			return errStreamRevoked
		}
		if st.Role != view.Role || !sameAccess(st.Access, view.Access) {
			view.Role, view.Access = st.Role, st.Access
			return write("event: access\ndata: {}\n\n")
		}
		return nil
	}
	drain := func() error {
		// Read before the pages: see feedHighWater.
		hw, err := s.svc.LatestEventSeq(ctx, o.ID)
		if err != nil {
			return err
		}
		for {
			if s.eventPageHook != nil {
				s.eventPageHook(last)
			}
			if err := recheck(); err != nil {
				return err
			}
			evs, err := s.svc.ListEvents(ctx, &view, last, int32(pageSize))
			if err != nil {
				return err
			}
			for _, ev := range evs {
				data, err := json.Marshal(ev)
				if err != nil {
					return err
				}
				if err := write("id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, data); err != nil {
					return err
				}
				last = ev.Seq
			}
			if len(evs) < pageSize {
				// The cursor moves past the rows this caller may not read, so
				// the next drain does not read them again. No id is sent:
				// Last-Event-ID stays the last event the caller received.
				last = max(last, hw)
				return nil
			}
		}
	}

	if err := write("retry: 3000\n: connected at seq %d\n\n", last); err != nil {
		return
	}
	if err := drain(); err != nil {
		s.logStreamEnd(r, err)
		return
	}
	heartbeat := time.NewTicker(s.SSEHeartbeat)
	defer heartbeat.Stop()
	poll := time.NewTicker(s.SSEPoll)
	defer poll.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case <-s.broker.Done():
			return
		case <-wake:
			err = drain()
		case <-poll.C:
			err = drain() // re-checks the credentials and the access first
		case <-heartbeat.C:
			err = write(": ping\n\n")
		}
		if err != nil {
			s.logStreamEnd(r, err)
			return
		}
	}
}

// feedHighWater: why a reader may move its cursor to LatestEventSeq read
// BEFORE its page queries. events.Emit takes the org's counter row lock
// (NextEventSeq) and holds it to COMMIT, so per org the seqs commit in order:
// once seq M is committed, every smaller seq is committed or rolled back.
// LatestEventSeq is MAX(seq) of the committed rows, M. Each later query
// (READ COMMITTED, a newer snapshot) sees every event up to M. So when the
// pages are read to their end, every event <= M the caller may read was
// returned, and the rows up to M need never be read again.

// errStreamRevoked ends an event stream whose credentials are gone; it is
// not logged as a failure.
var errStreamRevoked = errors.New("event stream: credentials no longer valid")

// sameAccess reports two team access sets as equal (IDs are sorted).
func sameAccess(a, b auth.TeamAccess) bool { return a.All == b.All && slices.Equal(a.IDs, b.IDs) }

func (s *Server) logStreamEnd(r *http.Request, err error) {
	if r.Context().Err() == nil && !errors.Is(err, errStreamRevoked) {
		s.log.WarnContext(r.Context(), "event stream ended", "err", err)
	}
}
