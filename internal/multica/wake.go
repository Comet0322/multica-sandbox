package multica

import (
	"encoding/json"
	"net/http"
	"time"

	"sandbox-manager/internal/multica/daemonwire"

	"github.com/gorilla/websocket"
)

// Wake sockets. The stock daemon in a pod polls for tasks every 30 seconds
// and also opens a websocket on which the server can tell it a task is
// available. The manager serves that socket and uses it to dispatch: on its own
// timer, while a pod with a socket is free, it claims a task upstream, places
// it as a poll would, and sends the target pod the same hint Multica would, so
// the pod claims at once. Polling stays as the fallback and the health signal:
// a pod without a socket, or a lost hint, is served by its next poll.
//
// The manager never acknowledges the daemon's websocket heartbeats, so the
// daemon keeps claiming over HTTP at its normal polling interval.

const (
	// wakePingEvery keeps the socket alive and detects a pod that vanished.
	wakePingEvery = 20 * time.Second
	wakeWriteWait = 10 * time.Second
	// wakeReadLimit bounds the frames a pod may send; the daemon only sends
	// small heartbeats, which are ignored.
	wakeReadLimit = 1 << 20
	// wakeSendBuffer is how many hints may queue for one pod. A hint carries
	// no content, so extras are dropped.
	wakeSendBuffer = 2
)

var wakeUpgrader = websocket.Upgrader{
	// Pods are identified by their pod token and are not browsers.
	CheckOrigin: func(*http.Request) bool { return true },
}

// wakeFrame is the content-free hint the daemon reacts to by claiming.
var wakeFrame = func() []byte {
	payload, _ := json.Marshal(daemonwire.TaskAvailablePayload{})
	frame, _ := json.Marshal(daemonwire.Message{Type: daemonwire.EventDaemonTaskAvailable, Payload: payload})
	return frame
}()

// workspacesChangedFrame tells a daemon to re-read its workspace list at once.
// Without it a pod that already has a runtime looks again only every 30 minutes,
// so a workspace added to the account would stay without a runtime that long.
var workspacesChangedFrame = func() []byte {
	frame, _ := json.Marshal(daemonwire.Message{Type: daemonwire.EventDaemonWorkspacesChanged})
	return frame
}()

// wakeConn is one pod's open socket.
type wakeConn struct {
	conn *websocket.Conn
	send chan []byte
	// changed carries the workspaces_changed hint apart from the wakes.
	changed chan struct{}
}

// serveWake upgrades a registered pod's connection and keeps it until either
// side closes. The daemon retries with backoff, so one that connects before it
// registered succeeds a little later.
func (a *DaemonAPI) serveWake(w http.ResponseWriter, r *http.Request, podID string) {
	if !a.cfg.Wake {
		http.NotFound(w, r)
		return
	}
	conn, err := wakeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already answered
	}
	wc := &wakeConn{conn: conn, send: make(chan []byte, wakeSendBuffer), changed: make(chan struct{}, 1)}
	a.wakeMu.Lock()
	if old := a.wakers[podID]; old != nil {
		_ = old.conn.Close() // the pod reconnected; the old socket is stale
	}
	a.wakers[podID] = wc
	a.wakeMu.Unlock()
	defer func() {
		a.wakeMu.Lock()
		if a.wakers[podID] == wc {
			delete(a.wakers, podID)
		}
		a.wakeMu.Unlock()
		_ = conn.Close()
	}()

	conn.SetReadLimit(wakeReadLimit)
	const readDeadline = 3 * wakePingEvery
	_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(readDeadline)) })
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			// The daemon's heartbeats are ignored: the HTTP heartbeat stays
			// the liveness signal.
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
		}
	}()
	a.cfg.Log.Info("pod opened its wake socket", "pod_id", podID)

	ping := time.NewTicker(wakePingEvery)
	defer ping.Stop()
	write := func(frame []byte) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(wakeWriteWait))
		return conn.WriteMessage(websocket.TextMessage, frame) == nil
	}
	for {
		// A pending workspaces_changed hint goes out before any wake.
		select {
		case <-wc.changed:
			if !write(workspacesChangedFrame) {
				return
			}
			continue
		default:
		}
		select {
		case <-wc.changed:
			if !write(workspacesChangedFrame) {
				return
			}
		case frame := <-wc.send:
			if !write(frame) {
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wakeWriteWait)); err != nil {
				return
			}
		case <-readDone:
			return
		}
	}
}

// wakePod tells the pod a task is waiting for it. It never blocks, and does
// nothing for a pod without a socket (its next poll finds the task).
func (a *DaemonAPI) WakePod(podID string) {
	a.wakeMu.Lock()
	wc := a.wakers[podID]
	a.wakeMu.Unlock()
	if wc == nil {
		return
	}
	select {
	case wc.send <- wakeFrame:
	default:
	}
}

// notifyWorkspacesChanged sends every connected pod the hint to re-read its
// workspace list. Like wakePod it never blocks. The hint has its own one-slot
// signal, sent before any wake, so a full wake buffer cannot drop it.
func (a *DaemonAPI) notifyWorkspacesChanged() {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	for _, wc := range a.wakers {
		if wc.changed == nil {
			// A socket made without the signal: share the wake buffer.
			select {
			case wc.send <- workspacesChangedFrame:
			default:
			}
			continue
		}
		select {
		case wc.changed <- struct{}{}:
		default: // one is pending already
		}
	}
}

// notifyEvery bounds how often notifyWorkspacesChangedLimited sends the hint.
const notifyEvery = 30 * time.Second

// notifyWorkspacesChangedLimited sends the workspaces_changed hint at most once
// per notifyEvery, for repairs that may repeat every pass (a registration that
// keeps failing, a runtime gone upstream).
func (a *DaemonAPI) NotifyWorkspacesChangedLimited() {
	a.notifyMu.Lock()
	now := a.now()
	if !a.notifiedAt.IsZero() && now.Sub(a.notifiedAt) < notifyEvery {
		a.notifyMu.Unlock()
		return
	}
	a.notifiedAt = now
	a.notifyMu.Unlock()
	a.notifyWorkspacesChanged()
}

// notifyWorkspacesChangedNow sends the hint at once and restarts the rate
// limit, for a change members are waiting on (a workspace joined or left).
func (a *DaemonAPI) NotifyWorkspacesChangedNow() {
	a.notifyMu.Lock()
	a.notifiedAt = a.now()
	a.notifyMu.Unlock()
	a.notifyWorkspacesChanged()
}

func (a *DaemonAPI) Wakeable(podID string) bool {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	return a.wakers[podID] != nil
}
