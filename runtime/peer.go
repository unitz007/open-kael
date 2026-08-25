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
	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/llm"
	"github.com/unitz007/kael/tools"
)

// dispatchTimeout bounds how long a dispatch call waits for a result once a
// task has actually been handed to a connected peer — generous, since real
// work on the far side (exploring, editing, building, testing) can
// legitimately run long.
const dispatchTimeout = 10 * time.Minute

// reconnectBackoff is how long MaintainPeer waits before redialing after a
// dropped connection.
const reconnectBackoff = 2 * time.Second

// pingInterval/pongWait are the WebSocket-level keepalive: without this, a
// connection that dies uncleanly on one side (network drop, laptop sleep —
// anything that doesn't send a proper close frame) can sit registered on
// the other side for a very long time, since the underlying OS TCP stack's
// own dead-connection detection is far slower than this. pongWait must
// exceed pingInterval by enough margin that one missed pong isn't treated
// as dead — confirmed live: a real network blip left a peer "connected"
// with no way to detect it short of this.
const (
	pingInterval = 20 * time.Second
	pongWait     = 45 * time.Second
)

// PeerInfo is one delegate target a connected peer has announced it hosts.
type PeerInfo struct {
	AgentID, AgentName, AgentDescription, AgentCapabilities string
}

// peerFrame is the one wire message shape, carried both directions once a
// connection is established — symmetric, since the same type of Runtime
// sits on both ends. Deliberately minimal: no credential or "extra
// payload" field. A caller needing to pass something like that (a GitHub
// token, say) is a separate, not-yet-built concern layered on top of this.
type peerFrame struct {
	Type    string     `json:"type"` // "register" | "task" | "result"
	Agents  []PeerInfo `json:"agents,omitempty"`   // register only: everything this side hosts
	TaskID  string     `json:"task_id,omitempty"`
	AgentID string     `json:"agent_id,omitempty"` // task only: which of the far side's local agents this is for
	Text    string     `json:"text,omitempty"`
	Result  string     `json:"result,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// Peer is one live connection to another Runtime — the same type, same
// handshake, same duplex frame loop, regardless of which side dialed and
// which side accepted. Registers itself onto local so local.DelegateTargets()
// immediately starts including the far side's agents, and routes any
// incoming task frame to whichever of local's own agents it names.
type Peer struct {
	ctx     context.Context
	ws      *websocket.Conn
	local   *Runtime
	remote  []PeerInfo
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan peerFrame
	done    chan struct{} // closed once, by readLoop's own cleanup, to stop pingLoop
}

// RemoteAgents reports every delegate target this peer announced it hosts
// at connect time.
func (p *Peer) RemoteAgents() []PeerInfo {
	return p.remote
}

func (p *Peer) send(f peerFrame) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.ws.WriteJSON(f)
}

// newPeer performs the symmetric handshake — send our own local.agentRegistry
// as a "register" frame, read the far side's — then registers itself onto
// local.peers. Does not itself start the read loop; the caller (Dial or
// Handler) does that once it has a *Peer, so both can block on it the same
// way regardless of which side established the connection.
func newPeer(ctx context.Context, ws *websocket.Conn, local *Runtime) (*Peer, error) {
	p := &Peer{ctx: ctx, ws: ws, local: local, pending: make(map[string]chan peerFrame), done: make(chan struct{})}

	mine := make([]PeerInfo, 0, len(local.agentRegistry))
	for _, a := range local.agentRegistry {
		mine = append(mine, PeerInfo{
			AgentID:           a.DelegateID(),
			AgentName:         a.DelegateName(),
			AgentDescription:  a.DelegateDescription(),
			AgentCapabilities: a.DelegateCapabilities(),
		})
	}
	if err := p.send(peerFrame{Type: "register", Agents: mine}); err != nil {
		return nil, fmt.Errorf("runtime: sending register frame: %w", err)
	}

	var f peerFrame
	if err := ws.ReadJSON(&f); err != nil {
		return nil, fmt.Errorf("runtime: reading register frame: %w", err)
	}
	if f.Type != "register" {
		return nil, fmt.Errorf("runtime: expected register frame, got %q", f.Type)
	}
	p.remote = f.Agents

	// Keepalive: a periodic ping (pingLoop, started below) resets this
	// deadline every time a pong actually arrives; if the connection is
	// dead, no pong comes and ReadJSON in readLoop eventually fails with a
	// timeout instead of blocking indefinitely.
	ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	go p.pingLoop()

	local.peersMu.Lock()
	local.replacePeerLocked(p)
	local.peersMu.Unlock()

	local.rememberRemotes(p.remote)
	local.drainQueueFor(p)

	return p, nil
}

// rememberRemotes records every agent id in infos into knownRemotes,
// keeping the newest-seen PeerInfo for each — this is what lets
// DelegateTargets keep offering a queued delegate tool for an id that has
// disconnected, using its most recently announced name/description/
// capabilities rather than nothing at all.
func (r *Runtime) rememberRemotes(infos []PeerInfo) {
	r.knownRemotesMu.Lock()
	defer r.knownRemotesMu.Unlock()
	if r.knownRemotes == nil {
		r.knownRemotes = make(map[string]PeerInfo, len(infos))
	}
	for _, info := range infos {
		r.knownRemotes[info.AgentID] = info
	}
}

// drainQueueFor re-dispatches every task queued for each agent id p just
// announced — called right after p becomes live for delegation (see
// newPeer), so a task queued while that id was offline runs as soon as
// it's reachable again, with no separate poll or retry loop needed. No-op
// if no TaskQueue is configured. Each task's re-dispatch runs in its own
// goroutine so one slow task can't hold up draining the rest, or block
// newPeer's own caller (Dial/Handler) from moving on to readLoop.
func (r *Runtime) drainQueueFor(p *Peer) {
	if r.queue == nil {
		return
	}
	for _, info := range p.remote {
		tasks, err := r.queue.Drain(p.ctx, info.AgentID)
		if err != nil {
			log.Printf("runtime: draining queued tasks for %s: %v", info.AgentID, err)
			continue
		}
		for _, task := range tasks {
			go func(task PendingTask) {
				result, err := p.dispatch(task.TargetID, task.ID, task.Task)
				if r.OnQueueDrained != nil {
					r.OnQueueDrained(p.ctx, task, result, err)
				}
			}(task)
		}
	}
}

// pingLoop sends a WebSocket ping every pingInterval until p.done closes —
// see pingInterval/pongWait's own doc comment for why this exists at all.
func (p *Peer) pingLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.writeMu.Lock()
			err := p.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			p.writeMu.Unlock()
			if err != nil {
				return // readLoop's own read deadline will notice the dead connection
			}
		case <-p.done:
			return
		}
	}
}

// readLoop blocks reading frames until the connection dies — the read
// erroring is the actual, immediate signal the peer is gone (closed,
// crashed, lost network), not an inferred one — at which point this peer
// is deregistered (so DelegateTargets() stops including it right away) and
// every task still waiting on this connection fails immediately instead of
// running out its own timeout.
func (p *Peer) readLoop() {
	defer func() {
		p.local.peersMu.Lock()
		for i, peer := range p.local.peers {
			if peer == p {
				p.local.peers = append(p.local.peers[:i], p.local.peers[i+1:]...)
				break
			}
		}
		p.local.peersMu.Unlock()
		close(p.done) // stops pingLoop
		p.failAllPending(fmt.Errorf("runtime: peer connection lost"))
		p.ws.Close()
	}()

	for {
		var f peerFrame
		if err := p.ws.ReadJSON(&f); err != nil {
			return
		}
		switch f.Type {
		case "task":
			// Handled in its own goroutine so a slow local agent doesn't
			// block reading further frames on this connection — including
			// "result" frames answering whatever this side has itself
			// dispatched to the far side concurrently.
			go p.handleTask(f)
		case "result":
			p.mu.Lock()
			ch, ok := p.pending[f.TaskID]
			if ok {
				delete(p.pending, f.TaskID)
			}
			p.mu.Unlock()
			if ok {
				ch <- f
			}
		}
	}
}

// handleTask runs an incoming task against whichever of local's own agents
// it names, and sends the result back.
func (p *Peer) handleTask(f peerFrame) {
	var target *agent.Agent
	for _, a := range p.local.agentRegistry {
		if a.DelegateID() == f.AgentID {
			target = a
			break
		}
	}

	out := peerFrame{Type: "result", TaskID: f.TaskID}
	if target == nil {
		out.Error = fmt.Sprintf("runtime: no local agent %q", f.AgentID)
	} else if result, err := target.RunDelegatedTask(p.ctx, f.Text); err != nil {
		out.Error = err.Error()
	} else {
		out.Result = result.Content
	}

	if err := p.send(out); err != nil {
		log.Println("runtime: sending result frame:", err)
	}
}

// evict force-closes p — called by replacePeerLocked when a fresh
// connection has just superseded it. Deliberately doesn't touch p.local.peers
// or p.done itself: closing p.ws unblocks p's own readLoop (still running
// in its own goroutine), and *that* runs the usual deferred cleanup
// (closing p.done, removing itself from peers — a no-op here, since
// replacePeerLocked already filtered it out).
func (p *Peer) evict() {
	p.failAllPending(fmt.Errorf("runtime: superseded by a new connection"))
	p.ws.Close()
}

func (p *Peer) failAllPending(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.pending {
		ch <- peerFrame{TaskID: id, Error: err.Error()}
		delete(p.pending, id)
	}
}

// dispatch sends a task to one of the far side's announced agents and
// blocks for the result, failing immediately if the connection drops
// before a reply arrives, or after dispatchTimeout if it never does.
func (p *Peer) dispatch(agentID, taskID, text string) (string, error) {
	ch := make(chan peerFrame, 1)
	p.mu.Lock()
	p.pending[taskID] = ch
	p.mu.Unlock()

	if err := p.send(peerFrame{Type: "task", TaskID: taskID, AgentID: agentID, Text: text}); err != nil {
		p.mu.Lock()
		delete(p.pending, taskID)
		p.mu.Unlock()
		return "", fmt.Errorf("runtime: sending task to %s: %w", agentID, err)
	}

	select {
	case f := <-ch:
		if f.Error != "" {
			return "", fmt.Errorf("runtime: %s", f.Error)
		}
		return f.Result, nil
	case <-time.After(dispatchTimeout):
		p.mu.Lock()
		delete(p.pending, taskID)
		p.mu.Unlock()
		return "", fmt.Errorf("runtime: %s timed out", agentID)
	}
}

// remoteDelegate implements agent.DelegateTarget for one agent a connected
// peer announced it hosts — a minimal routing handle, not a second
// in-process agent. The one real agent is whatever is registered on the
// far side's own Runtime; this type's only job is forwarding
// RunDelegatedTask over the connection and relaying back whatever result
// comes back.
type remoteDelegate struct {
	info PeerInfo
	peer *Peer
}

func (r *remoteDelegate) DelegateID() string           { return r.info.AgentID }
func (r *remoteDelegate) DelegateName() string         { return r.info.AgentName }
func (r *remoteDelegate) DelegateDescription() string  { return r.info.AgentDescription }
func (r *remoteDelegate) DelegateCapabilities() string { return r.info.AgentCapabilities }

func (r *remoteDelegate) RunDelegatedTask(ctx context.Context, task string) (*agent.LoopResult, error) {
	result, _, err := (&networkLoop{peer: r.peer, agentID: r.info.AgentID}).Run(ctx, []llm.Message{{Role: "user", Content: task}}, nil)
	return result, err
}

// networkLoop is an agent.AgentLoop backed by dispatching a task over a
// Peer connection and waiting for its result — the network-dispatching
// counterpart to CLILoop's local-subprocess one, same shape.
type networkLoop struct {
	peer    *Peer
	agentID string
}

func (n *networkLoop) Run(ctx context.Context, messages []llm.Message, _ []*tools.ToolSpec) (*agent.LoopResult, []llm.Message, error) {
	task := agent.LastUserMessage(messages)
	result, err := n.peer.dispatch(n.agentID, newTaskID(n.agentID), task)
	if err != nil {
		return nil, messages, err
	}
	final := append(messages, llm.Message{Role: "assistant", Content: result})
	return &agent.LoopResult{Status: agent.LLMStatusComplete, Content: result}, final, nil
}

var (
	taskIDMu      sync.Mutex
	taskIDCounter uint64
)

// newTaskID builds a unique task identifier for one dispatch call, scoped
// under agentID purely for readability in logs — uniqueness comes from the
// counter, not the prefix.
func newTaskID(agentID string) string {
	taskIDMu.Lock()
	defer taskIDMu.Unlock()
	taskIDCounter++
	return fmt.Sprintf("%s-%d", agentID, taskIDCounter)
}

var upgrader = websocket.Upgrader{
	// No origin check needed — this isn't served to browsers, only to
	// another Runtime dialing in directly.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Dial connects out to addr as a new Peer and blocks running its read loop
// until the connection drops. Call in a reconnect loop for a long-lived
// connection — see MaintainPeer.
func Dial(ctx context.Context, addr, token string, local *Runtime) error {
	wsURL, err := toWebSocketURL(addr)
	if err != nil {
		return err
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("token rejected: %w", err)
		}
		return err
	}
	defer ws.Close()

	p, err := newPeer(ctx, ws, local)
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		ws.Close()
	}()

	p.readLoop()
	return nil
}

// Handler upgrades an authenticated HTTP connection into a Peer the same
// way Dial does from the other side, and blocks running its read loop
// until the connection drops. Mount at whatever path your app chooses —
// the far side's addr (passed to Dial) needs to include that same path.
func Handler(token string, local *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("runtime: upgrade failed:", err)
			return
		}
		p, err := newPeer(r.Context(), ws, local)
		if err != nil {
			log.Println("runtime: peer handshake failed:", err)
			ws.Close()
			return
		}
		p.readLoop()
	}
}

// MaintainPeer wraps Dial in a reconnect-with-backoff loop — for anything
// that needs a long-lived outbound peer connection, reconnecting
// automatically on any drop, until ctx is cancelled. Blocks.
func MaintainPeer(ctx context.Context, addr, token string, local *Runtime) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := Dial(ctx, addr, token, local); err != nil {
			log.Println("runtime: peer connection error, reconnecting:", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectBackoff):
		}
	}
}

// authorized checks a bearer token on the upgrade request.
func authorized(r *http.Request, token string) bool {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	got = strings.TrimPrefix(got, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// toWebSocketURL turns an http(s) address into its ws(s) counterpart,
// accepting either scheme so addr can just be a normal https:// URL rather
// than requiring the caller to know it needs to say "wss://" instead. The
// path is left untouched — this package has no opinion on what route a
// Handler is mounted at; addr must already include it.
func toWebSocketURL(addr string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("runtime: invalid address %q: %w", addr, err)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("runtime: address %q must start with http(s):// or ws(s)://", addr)
	}
	return u.String(), nil
}
