package runtime

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/unitz007/open-kael/domain"
)

// Peer is the framework's answer to "hand a task to a remote worker and
// wait for the result" — a simplified version of the same idea Kael's own
// distributed runtime uses (kael-platform's runtime.Peer): a named worker
// process dials OUT to this one over a WebSocket (since it's typically the
// worker — a laptop behind NAT — that can't be dialed INTO, not the
// server), keeps that connection open, and tasks travel over it in both
// directions with no polling. Simplified relative to Kael's version: one
// named peer at a time, task dispatch only — no multi-agent capability
// discovery, since a caller here already knows which peer it wants.
const (
	peerPingInterval     = 20 * time.Second
	peerPongWait         = 45 * time.Second
	peerDispatchTimeout  = 10 * time.Minute
	peerReconnectBackoff = 2 * time.Second
	// peerSideCallTimeout bounds one mid-task side call (e.g. posting a
	// status update) — much shorter than peerDispatchTimeout, since a side
	// call is a quick aside, not the task's own real work. A hung side call
	// shouldn't be able to hang the whole task.
	peerSideCallTimeout = 30 * time.Second
)

type peerFrameType string

const (
	frameHello      peerFrameType = "hello"
	frameTask       peerFrameType = "task"
	frameResult     peerFrameType = "result"
	frameSideCall   peerFrameType = "side_call"
	frameSideResult peerFrameType = "side_result"
)

// peerFrame is the one wire message shape, carried both directions over an
// established connection. SideCallID/Action/Input are only set on
// frameSideCall/frameSideResult — a task in flight can call back to the hub
// zero or more times before its own frameResult, correlated by TaskID plus
// its own SideCallID (a task can have several side calls outstanding at
// once, e.g. two post_update calls issued close together).
type peerFrame struct {
	Type   peerFrameType `json:"type"`
	PeerID string        `json:"peer_id,omitempty"`
	TaskID string        `json:"task_id,omitempty"`
	Text   string        `json:"text,omitempty"`
	Error  string        `json:"error,omitempty"`
	// Actions is only set on frameTask — the dispatcher's own BoundActions,
	// described as plain specs (name/description/schema, no Invoke func;
	// that can't cross the wire) for the peer to expose however it sees
	// fit (e.g. as dynamic MCP tools for an agentic CLI process to call).
	// Calling one back is just a frameSideCall/frameSideResult round trip
	// like any other — Actions only tells the peer what names/schemas are
	// available to call, it doesn't change the side-call mechanism itself.
	Actions []domain.ActionSpec `json:"actions,omitempty"`

	SideCallID string `json:"side_call_id,omitempty"`
	Action     string `json:"action,omitempty"` // frameSideCall only
	// Input carries the side call's arguments on a frameSideCall, and the
	// handler's return value on the matching frameSideResult — one field,
	// repurposed by direction, same as Text/Error already are for
	// frameTask/frameResult.
	Input map[string]any `json:"input,omitempty"`
}

// SideCallHandler answers a side call a peer makes while a task it's
// currently running is still in flight — e.g. Claude Code deciding to post
// a status update mid-task via an MCP tool, relayed by the worker process
// back to whoever dispatched the task. Framework-generic: PeerHub has no
// idea what "post_update" means, it just carries the action/input to
// whichever handler Dispatch's caller supplied and carries the answer back.
type SideCallHandler func(ctx context.Context, action string, input map[string]any) (map[string]any, error)

// pendingTask is what a Peer tracks for one in-flight Dispatch call: where
// to deliver the eventual frameResult, and how to answer any frameSideCall
// frames that arrive for this TaskID before that.
type pendingTask struct {
	result chan peerFrame
	onSide SideCallHandler
	ctx    context.Context
}

// Peer is one connected remote worker's server-side handle.
type Peer struct {
	id      string
	conn    *websocket.Conn
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]*pendingTask
}

func (p *Peer) send(f peerFrame) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.conn.WriteJSON(f)
}

// PeerHub is the server-side registry of connected peers, and the
// dispatch entrypoint a domain.Executor calls to hand a task to one (see
// this repo's examples or a consumer's own Executor for the pattern —
// PeerHub itself doesn't know or care what a "task" means, it just carries
// text to a named peer and carries text back).
type PeerHub struct {
	mu    sync.RWMutex
	peers map[string]*Peer
}

func NewPeerHub() *PeerHub {
	return &PeerHub{peers: make(map[string]*Peer)}
}

func (h *PeerHub) register(p *Peer) {
	h.mu.Lock()
	h.peers[p.id] = p
	h.mu.Unlock()
}

func (h *PeerHub) unregister(id string) {
	h.mu.Lock()
	delete(h.peers, id)
	h.mu.Unlock()
}

func (h *PeerHub) get(id string) (*Peer, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	p, ok := h.peers[id]
	return p, ok
}

// Connected reports whether a named peer currently has a live connection.
func (h *PeerHub) Connected(peerID string) bool {
	_, ok := h.get(peerID)
	return ok
}

// Dispatch hands text to peerID as a task and blocks until it responds,
// ctx is cancelled, or peerDispatchTimeout elapses — whichever comes
// first. Generous timeout since real work on the far side (Kael's own
// dispatchTimeout comment: "exploring, editing, building, testing") can
// legitimately run long. onSideCall answers any side calls the peer makes
// while this task is in flight (see SideCallHandler) — nil means any side
// call attempted against this task is refused (see handleSideCall). actions
// is optional (nil is fine) — specs the peer may expose as callable
// actions for this one task; see peerFrame.Actions.
func (h *PeerHub) Dispatch(ctx context.Context, peerID, text string, actions []domain.ActionSpec, onSideCall SideCallHandler) (string, error) {
	p, ok := h.get(peerID)
	if !ok {
		return "", fmt.Errorf("peer %q is not connected", peerID)
	}

	taskID := fmt.Sprintf("%s-%d", peerID, time.Now().UnixNano())
	task := &pendingTask{result: make(chan peerFrame, 1), onSide: onSideCall, ctx: ctx}
	p.pendingMu.Lock()
	p.pending[taskID] = task
	p.pendingMu.Unlock()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, taskID)
		p.pendingMu.Unlock()
	}()

	if err := p.send(peerFrame{Type: frameTask, TaskID: taskID, Text: text, Actions: actions}); err != nil {
		return "", fmt.Errorf("dispatch to peer %q: %w", peerID, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, peerDispatchTimeout)
	defer cancel()

	select {
	case f := <-task.result:
		if f.Error != "" {
			return "", fmt.Errorf("peer %q: %s", peerID, f.Error)
		}
		return f.Text, nil
	case <-waitCtx.Done():
		return "", fmt.Errorf("dispatch to peer %q: %w", peerID, waitCtx.Err())
	}
}

var peerUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Origin checking isn't the security boundary here — the bearer token
	// in Handler is. A peer connection is worker-to-server, not
	// browser-to-server, so there's no browser Origin header to trust or
	// distrust in the first place.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Handler upgrades an incoming request to a peer connection, authenticating
// via a shared bearer token before accepting — mount it on whatever route
// a worker process's Dial/MaintainPeer call targets.
func (h *PeerHub) Handler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !peerAuthorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := peerUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		h.handleConn(conn)
	}
}

func peerAuthorized(r *http.Request, token string) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	provided := strings.TrimPrefix(auth, prefix)
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func (h *PeerHub) handleConn(conn *websocket.Conn) {
	defer conn.Close()

	var hello peerFrame
	if err := conn.ReadJSON(&hello); err != nil || hello.Type != frameHello || hello.PeerID == "" {
		return
	}

	p := &Peer{id: hello.PeerID, conn: conn, pending: make(map[string]*pendingTask)}
	h.register(p)
	defer h.unregister(p.id)

	conn.SetReadDeadline(time.Now().Add(peerPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(peerPongWait))
		return nil
	})

	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		ticker := time.NewTicker(peerPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.writeMu.Lock()
				err := conn.WriteMessage(websocket.PingMessage, nil)
				p.writeMu.Unlock()
				if err != nil {
					return
				}
			case <-stopPing:
				return
			}
		}
	}()

	for {
		var f peerFrame
		if err := conn.ReadJSON(&f); err != nil {
			return
		}
		switch f.Type {
		case frameResult:
			p.pendingMu.Lock()
			task, ok := p.pending[f.TaskID]
			p.pendingMu.Unlock()
			if ok {
				task.result <- f
			}
		case frameSideCall:
			// Handled in its own goroutine — onSide may block on real work
			// (e.g. posting to Teams), and this read loop must keep
			// servicing other frames on this connection meanwhile,
			// including the eventual frameResult for this same task.
			go p.handleSideCall(f)
		}
	}
}

// handleSideCall answers one frameSideCall by running the task's own
// SideCallHandler (set when Dispatch was called) and writing back a
// frameSideResult with the same TaskID/SideCallID. A task with no handler
// configured, or a side call for a task this Peer no longer knows about
// (already finished, or never dispatched from here), gets a clear error
// back rather than silently hanging the caller.
func (p *Peer) handleSideCall(f peerFrame) {
	p.pendingMu.Lock()
	task, ok := p.pending[f.TaskID]
	p.pendingMu.Unlock()

	resp := peerFrame{Type: frameSideResult, TaskID: f.TaskID, SideCallID: f.SideCallID}
	switch {
	case !ok:
		resp.Error = fmt.Sprintf("side call for unknown or already-finished task %q", f.TaskID)
	case task.onSide == nil:
		resp.Error = fmt.Sprintf("task %q accepts no side calls", f.TaskID)
	default:
		output, err := task.onSide(task.ctx, f.Action, f.Input)
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Input = output
		}
	}
	if err := p.send(resp); err != nil {
		log.Printf("runtime: peer %q: failed to answer side call %q: %v", p.id, f.SideCallID, err)
	}
}

// SideCaller lets a TaskHandler call back to the hub while its task is
// still in flight — the client-side mirror of SideCallHandler. Blocks until
// the hub answers, ctx is cancelled, or peerSideCallTimeout elapses.
type SideCaller func(ctx context.Context, action string, input map[string]any) (map[string]any, error)

// TaskHandler is called on the worker/client side for each task the hub
// dispatches — return the result text, or an error to report back to
// whichever Dispatch call is waiting. sideCall lets the handler post
// updates back to the hub before it's done (e.g. Claude Code deciding to
// report progress mid-task via an MCP tool) — calling it is entirely
// optional, most tasks never will. actions is whatever Dispatch's own
// caller passed as its own actions parameter — empty for most tasks; a
// non-empty list means the handler can expose them (e.g. as dynamic MCP
// tools) and route calls back via sideCall using the matching Action name.
type TaskHandler func(ctx context.Context, taskID, text string, actions []domain.ActionSpec, sideCall SideCaller) (string, error)

// Dial connects once to addr as peerID, authenticating with token, and
// serves incoming tasks via handle until the connection drops or ctx is
// cancelled. Returns nil on a clean ctx-cancelled shutdown; any other
// return is a real connection failure, which MaintainPeer treats as a
// signal to redial.
func Dial(ctx context.Context, addr, token, peerID string, handle TaskHandler) error {
	wsURL, err := toWebSocketURL(addr)
	if err != nil {
		return err
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(peerFrame{Type: frameHello, PeerID: peerID}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(peerPongWait))
	conn.SetPingHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(peerPongWait))
		return conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(5*time.Second))
	})

	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closed:
		}
	}()
	defer close(closed)

	var writeMu sync.Mutex
	writeJSON := func(f peerFrame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(f)
	}

	var pendingSideMu sync.Mutex
	pendingSide := make(map[string]chan peerFrame)

	// sideCall is handed to every TaskHandler invocation on this
	// connection — mirrors PeerHub.Dispatch's own pending-channel-keyed-by-ID
	// pattern, direction reversed: this side initiates, the hub answers.
	sideCall := func(ctx context.Context, taskID, action string, input map[string]any) (map[string]any, error) {
		sideCallID := fmt.Sprintf("%s-side-%d", taskID, time.Now().UnixNano())
		ch := make(chan peerFrame, 1)
		pendingSideMu.Lock()
		pendingSide[sideCallID] = ch
		pendingSideMu.Unlock()
		defer func() {
			pendingSideMu.Lock()
			delete(pendingSide, sideCallID)
			pendingSideMu.Unlock()
		}()

		if err := writeJSON(peerFrame{Type: frameSideCall, TaskID: taskID, SideCallID: sideCallID, Action: action, Input: input}); err != nil {
			return nil, fmt.Errorf("side call: %w", err)
		}

		waitCtx, cancel := context.WithTimeout(ctx, peerSideCallTimeout)
		defer cancel()

		select {
		case f := <-ch:
			if f.Error != "" {
				return nil, fmt.Errorf("side call: %s", f.Error)
			}
			return f.Input, nil
		case <-waitCtx.Done():
			return nil, fmt.Errorf("side call: %w", waitCtx.Err())
		}
	}

	for {
		var f peerFrame
		if err := conn.ReadJSON(&f); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		switch f.Type {
		case frameTask:
			go func(f peerFrame) {
				taskSideCall := func(ctx context.Context, action string, input map[string]any) (map[string]any, error) {
					return sideCall(ctx, f.TaskID, action, input)
				}
				result, err := handle(ctx, f.TaskID, f.Text, f.Actions, taskSideCall)
				resp := peerFrame{Type: frameResult, TaskID: f.TaskID, Text: result}
				if err != nil {
					resp.Error = err.Error()
				}
				_ = writeJSON(resp)
			}(f)
		case frameSideResult:
			pendingSideMu.Lock()
			ch, ok := pendingSide[f.SideCallID]
			pendingSideMu.Unlock()
			if ok {
				ch <- f
			}
		}
	}
}

// MaintainPeer calls Dial in a loop, reconnecting after peerReconnectBackoff
// on any connection failure, until ctx is cancelled.
func MaintainPeer(ctx context.Context, addr, token, peerID string, handle TaskHandler) error {
	for {
		if err := Dial(ctx, addr, token, peerID, handle); err != nil {
			log.Printf("runtime: peer %q: connection lost, reconnecting: %v", peerID, err)
		}
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(peerReconnectBackoff):
		}
	}
}

func toWebSocketURL(addr string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("parse addr %q: %w", addr, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("addr %q: unsupported scheme %q", addr, u.Scheme)
	}
	return u.String(), nil
}
