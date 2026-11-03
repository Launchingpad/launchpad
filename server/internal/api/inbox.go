package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/notify"
)

// The inbox routes (LAUN-35, docs/internal/decisions-T13-notifications.md
// #27-#31). Every route works on the caller's own rows; an agent token gets
// 403 forbidden (refuseAgents); another user's or org's id is 404.

const inboxStreamBatch = 100

func (s *Server) listInbox(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	q := r.URL.Query()
	in := notify.InboxQuery{Cursor: q.Get("cursor")}
	switch q.Get("state") {
	case "", "all":
	case "unread":
		in.UnreadOnly = true
	default:
		s.fail(w, r, httpx.Invalid("state must be unread or all"))
		return
	}
	switch q.Get("archived") {
	case "", "false":
	case "true":
		in.Archived = true
	default:
		s.fail(w, r, httpx.Invalid("archived must be true or false"))
		return
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > notify.InboxMaxLimit {
			s.fail(w, r, httpx.Invalid("limit must be 1-%d", notify.InboxMaxLimit))
			return
		}
		in.Limit = n
	}
	page, err := s.notify.ListInbox(r.Context(), o, p.UserID, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) inboxUnreadCount(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	n, err := s.notify.UnreadCount(r.Context(), o, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, notify.UnreadCount{Count: n})
}

// wakeInbox tells every stream of the org that an inbox row changed: this
// server's broker at once, and the other servers through NOTIFY. A failure
// costs latency only (the streams also poll).
func (s *Server) wakeInbox(r *http.Request, org uuid.UUID) {
	s.broker.Publish(org)
	if err := dbq.New(s.pool).NotifyEvents(r.Context(), org.String()); err != nil {
		s.log.WarnContext(r.Context(), "inbox wake-up not sent", "org_id", org, "err", err)
	}
}

func (s *Server) patchInbox(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in notify.InboxPatch
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, r, httpx.NotFound("notification"))
		return
	}
	it, err := s.notify.PatchInbox(r.Context(), o, p.UserID, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.wakeInbox(r, o.ID)
	httpx.WriteJSON(w, http.StatusOK, it)
}

func (s *Server) readAllInbox(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in notify.ReadAllInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.notify.ReadAll(r.Context(), o, p.UserID, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if res.Marked > 0 {
		s.wakeInbox(r, o.ID)
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// streamInbox: GET .../notifications/stream, Server-Sent Events, the same
// pattern as streamEvents but for the caller's own inbox rows (#31).
//
// `event: notification` / `id: <inboxSeq>` / `data: {id, kind, inboxSeq}` for
// each new inbox row (email-only and archived rows are never sent), and
// `event: count` / `data: {count}` whenever the unread count differs from the
// last one sent on this connection, and once at the start. A count event has
// no id, so it does not move Last-Event-ID. Resume point: Last-Event-ID,
// else ?after=, else "now". The handler wakes on the broker and polls every
// SSEPoll (a lost NOTIFY costs latency only). Before every page of every
// drain (the first one, a wake-up and a poll tick) it re-reads the token, the
// user, the membership, the role and the team access, as streamEvents does
// (T20 #56, #62): it ends the stream when any credential is gone, and sends
// `event: access` (data {}) and then reads with the new set when the role or
// the access changed. The rows and the count use the access last read.
func (s *Server) streamInbox(w http.ResponseWriter, r *http.Request) {
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
		n, err := s.notify.LatestInboxSeq(ctx, o, p.UserID)
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
	if err := rc.SetReadDeadline(time.Time{}); err != nil && err != http.ErrNotSupported {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
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
	lastCount := int64(-1)
	view := *o // the caller's role and access as this stream last read them
	recheck := func() error {
		st, ok, err := s.svc.StillAuthorized(ctx, p, o.ID)
		if err != nil {
			return err
		}
		if !ok {
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
		for {
			if err := recheck(); err != nil {
				return err
			}
			rows, err := s.notify.StreamRows(ctx, &view, p.UserID, last, inboxStreamBatch)
			if err != nil {
				return err
			}
			for _, row := range rows {
				data, err := json.Marshal(row)
				if err != nil {
					return err
				}
				if err := write("id: %d\nevent: notification\ndata: %s\n\n", row.InboxSeq, data); err != nil {
					return err
				}
				last = row.InboxSeq
			}
			if len(rows) < inboxStreamBatch {
				break
			}
		}
		// A read_at or archived_at change moves no inbox_seq: the count event
		// carries a mark-read, an unread, an archive or a read-all to the
		// other tabs.
		n, err := s.notify.UnreadCount(ctx, &view, p.UserID)
		if err != nil {
			return err
		}
		if n != lastCount {
			data, err := json.Marshal(notify.UnreadCount{Count: n})
			if err != nil {
				return err
			}
			if err := write("event: count\ndata: %s\n\n", data); err != nil {
				return err
			}
			lastCount = n
		}
		return nil
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
