// Package mcp is the tracker's Model Context Protocol server: newline-
// delimited JSON-RPC 2.0 over stdio (the MCP stdio transport), with tools
// that call the tracker REST API. The tool names and arguments follow
// Linear's MCP server (docs/internal/decisions-T2.md).
//
// The protocol layer is written here, on the standard library, instead of
// pulling in an SDK: the stdio transport needs four methods (initialize,
// ping, tools/list, tools/call) and two notifications.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol versions this server speaks, newest first. A client asking for
// one of them gets it back; any other gets the newest.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// maxLineBytes caps one incoming message. A longer line is refused on its
// own; the session goes on.
const maxLineBytes = 8 << 20

// maxInFlight bounds concurrent tool calls; maxQueued bounds the calls
// waiting for a slot. The read loop never waits for a slot, so a ping or a
// notifications/cancelled is read at once even with every slot busy.
const (
	maxInFlight = 8
	maxQueued   = 64
)

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeServerBusy     = -32000
)

// Server serves one MCP session.
type Server struct {
	tools   *Tools
	log     *slog.Logger
	name    string
	version string

	mu  sync.Mutex // guards out
	out *bufio.Writer

	maxLine int // maxLineBytes; tests lower it before Serve

	cmu      sync.Mutex // guards inflight
	inflight map[string]*inflightCall
}

// inflightCall is a tools/call still running.
type inflightCall struct {
	cancel context.CancelFunc
	// byClient is set by notifications/cancelled: the call then sends no
	// response (MCP: the receiver SHOULD NOT answer a cancelled request).
	byClient atomic.Bool
}

// NewServer wraps tools. log gets one line per call (tool name, outcome,
// duration); it never gets arguments, results or the token.
func NewServer(tools *Tools, log *slog.Logger, version string) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{tools: tools, log: log, name: "launchpad", version: version,
		inflight: map[string]*inflightCall{}, maxLine: maxLineBytes}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from r and writes responses to w until r ends or ctx
// is cancelled. At the end of r, in-flight calls finish and answer; when ctx
// is cancelled they are cancelled. Either way Serve waits for them.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.out = bufio.NewWriter(w)

	// The reader goroutine ends at the end of r, or, once ctx is done, after
	// its current read returns. Serve does not wait for it: a read blocked on
	// an open stdin must not keep a signalled process alive.
	limit := s.maxLine // read once: the reader goroutine may outlive Serve
	lines := make(chan inLine)
	readErr := make(chan error, 1) // sent only when r ends
	go func() {
		defer close(lines)
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			b, tooLong, err := readLine(br, limit)
			if len(bytes.TrimSpace(b)) > 0 || tooLong {
				select {
				case lines <- inLine{b: b, tooLong: tooLong}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				readErr <- err
				return
			}
		}
	}()

	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		var in inLine
		select {
		case <-ctx.Done():
			return ctx.Err()
		case l, ok := <-lines:
			if !ok {
				// The reader stopped: either r ended (readErr has the
				// outcome) or ctx was cancelled (readErr stays empty, so
				// waiting on it alone would hang a SIGTERM).
				select {
				case err := <-readErr:
					// stdin closed. The MCP stdio shutdown is "close stdin,
					// wait for the process to exit, then SIGTERM", so calls
					// still running finish and answer (the deferred
					// wg.Wait).
					if err != nil {
						return fmt.Errorf("read stdin: %w", err)
					}
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			in = l
		}
		if in.tooLong {
			s.reply(response{ID: json.RawMessage("null"), Error: &rpcError{codeInvalidRequest,
				fmt.Sprintf("message larger than %d bytes", limit)}})
			continue
		}
		line := bytes.TrimSpace(in.b)
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.reply(response{ID: json.RawMessage("null"), Error: &rpcError{codeParseError, "parse error"}})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			if req.ID != nil {
				s.reply(response{ID: req.ID, Error: &rpcError{codeInvalidRequest, "invalid request"}})
			}
			continue
		}
		if req.ID == nil { // a notification: never answered
			s.notify(req)
			continue
		}
		if req.Method != "tools/call" {
			s.reply(s.handle(ctx, req))
			continue
		}
		key := string(req.ID)
		s.cmu.Lock()
		_, dup := s.inflight[key]
		s.cmu.Unlock()
		if dup {
			s.reply(response{ID: req.ID, Error: &rpcError{codeInvalidRequest, "a request with this id is already in flight"}})
			continue
		}
		s.cmu.Lock()
		n := len(s.inflight)
		s.cmu.Unlock()
		if n >= maxInFlight+maxQueued {
			s.reply(response{ID: req.ID, Error: &rpcError{codeServerBusy, "too many tool calls in flight"}})
			continue
		}
		cctx, ccancel := context.WithCancel(ctx)
		call := &inflightCall{cancel: ccancel}
		s.cmu.Lock()
		s.inflight[key] = call
		s.cmu.Unlock()
		wg.Go(func() {
			defer func() {
				s.cmu.Lock()
				delete(s.inflight, key)
				s.cmu.Unlock()
				ccancel()
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-cctx.Done():
				// Cancelled (by the client or a signal) while queued.
				return
			}
			if cctx.Err() != nil {
				return // a slot and the cancel were ready together
			}
			resp := s.handle(cctx, req)
			if call.byClient.Load() {
				return // the client cancelled it: no answer
			}
			s.reply(resp)
		})
	}
}

// inLine is one line read from stdin.
type inLine struct {
	b       []byte
	tooLong bool
}

// readLine reads one '\n'-terminated line of at most max bytes. A longer
// line is read to its end and dropped, and reported as tooLong.
func readLine(br *bufio.Reader, max int) ([]byte, bool, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > max {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, tooLong, err
	}
}

func (s *Server) reply(resp response) {
	resp.JSONRPC = "2.0"
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(response{JSONRPC: "2.0", ID: resp.ID, Error: &rpcError{codeInternal, "encode response"}})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(b)
	_ = s.out.WriteByte('\n')
	if err := s.out.Flush(); err != nil {
		s.log.Error("write stdout", "err", err)
	}
}

func (s *Server) notify(req request) {
	if req.Method != "notifications/cancelled" {
		return // notifications/initialized and the rest need nothing
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.RequestID == nil {
		return
	}
	s.cmu.Lock()
	call := s.inflight[string(p.RequestID)]
	s.cmu.Unlock()
	if call != nil {
		call.byClient.Store(true)
		call.cancel()
	}
}

func (s *Server) handle(ctx context.Context, req request) response {
	resp := response{ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p) // a missing version gets the newest
		v := protocolVersions[0]
		for _, pv := range protocolVersions {
			if pv == p.ProtocolVersion {
				v = pv
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
			"instructions":    s.instructions(),
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.tools.list()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			resp.Error = &rpcError{codeInvalidParams, "tools/call needs a name"}
			return resp
		}
		tl, ok := s.tools.lookup(p.Name)
		if !ok {
			// Includes every write tool under the reviewer profile: nothing
			// is called, and the client is told the tool does not exist.
			s.log.Warn("unknown tool", "tool", p.Name, "profile", s.tools.profile)
			resp.Error = &rpcError{codeInvalidParams, fmt.Sprintf("unknown tool: %s", p.Name)}
			return resp
		}
		start := time.Now()
		out, err := s.tools.call(ctx, tl, p.Arguments)
		dur := time.Since(start).Round(time.Millisecond)
		if err != nil {
			s.log.Info("tool call", "tool", p.Name, "ok", false, "duration", dur, "status", statusOf(err))
			resp.Result = toolResult(err.Error(), true)
			return resp
		}
		b, err := json.Marshal(out)
		if err != nil {
			resp.Result = toolResult("encode result: "+err.Error(), true)
			return resp
		}
		s.log.Info("tool call", "tool", p.Name, "ok", true, "duration", dur)
		resp.Result = toolResult(string(b), false)
	default:
		resp.Error = &rpcError{codeMethodNotFound, "method not found: " + req.Method}
	}
	return resp
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

func statusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func (s *Server) instructions() string {
	if s.tools.profile == ProfileReviewer {
		return "Launchpad, the issue tracker, read-only reviewer profile: get_issue, list_comments and get_document only."
	}
	return "Launchpad, the issue tracker. Tool names and arguments follow Linear's MCP server. " +
		"List tools return {items, nextCursor}; pass nextCursor back as cursor. " +
		"File attachments: prepare_attachment_upload, PUT the file yourself, then create_attachment_from_upload. " +
		"The org's admins may have written working guidance for agents: call get_guidance at the start of work."
}
