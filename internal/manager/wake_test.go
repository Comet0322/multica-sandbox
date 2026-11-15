package manager_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/manager"

	"github.com/gorilla/websocket"
)

const wakeEvent = "daemon:task_available"

func withWake(d time.Duration) func(*manager.Config) {
	return func(c *manager.Config) { c.DispatchInterval = d }
}

// wakeSocket is a pod daemon's wake websocket.
type wakeSocket struct {
	t    *testing.T
	conn *websocket.Conn

	once   sync.Once
	frames chan []byte
}

// dialWake tries the daemon's websocket with the pod token and returns the
// handshake response.
func (p *simPod) dialWake() (*wakeSocket, *http.Response, error) {
	p.h.t.Helper()
	d := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}
	header := http.Header{"Authorization": {"Bearer " + p.token}}
	conn, resp, err := d.Dial("ws"+strings.TrimPrefix(p.h.podAPI.URL, "http")+"/api/daemon/ws", header)
	if err != nil {
		return nil, resp, err
	}
	p.h.t.Cleanup(func() { _ = conn.Close() })
	return &wakeSocket{t: p.h.t, conn: conn}, resp, nil
}

// openWake connects the pod's socket and waits until the manager holds it.
func (p *simPod) openWake() *wakeSocket {
	p.h.t.Helper()
	before := strings.Count(p.h.logs.String(), "pod opened its wake socket")
	ws, resp, err := p.dialWake()
	if err != nil {
		p.h.t.Fatalf("wake socket: %v (response %v)", err, resp)
	}
	eventually(p.h.t, "manager holds the socket", func() bool {
		return strings.Count(p.h.logs.String(), "pod opened its wake socket") > before
	})
	return ws
}

// next returns the type of the next frame, or "" if none arrives within d. A
// websocket read that times out cannot be retried, so a reader goroutine owns
// the connection once next is used and hands the frames over a channel.
func (w *wakeSocket) next(d time.Duration) string {
	w.t.Helper()
	w.once.Do(func() {
		w.frames = make(chan []byte, 64)
		go func() {
			defer close(w.frames)
			for {
				_, raw, err := w.conn.ReadMessage()
				if err != nil {
					return
				}
				w.frames <- raw
			}
		}()
	})
	select {
	case raw, ok := <-w.frames:
		if !ok {
			w.t.Fatalf("wake socket closed")
		}
		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			w.t.Fatalf("bad frame %q: %v", raw, err)
		}
		return msg.Type
	case <-time.After(d):
		return ""
	}
}

func (h *harness) dispatch() { h.pool.DispatchOnce(context.Background()) }

func TestWakeSocketUpgradesOnlyPodsWithAKnownToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	known := h.newPod("pool-0")
	known.start()
	stranger := &simPod{h: h, name: "stranger", token: "mpt_forged", client: &http.Client{}}

	ws, resp, err := known.dialWake()
	if err != nil {
		t.Fatalf("registered pod: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("registered pod: want 101, got %d", resp.StatusCode)
	}
	_ = ws.conn.Close()

	if _, resp, err := stranger.dialWake(); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown pod token: want 401, got %v (err %v)", resp, err)
	}
}

func TestWakeSocketIsOffWithoutWakeInterval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()

	if _, resp, err := pod.dialWake(); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DispatchInterval=0: want 404, got %v (err %v)", resp, err)
	}
	// The tick still claims; the pod picks its task up by polling.
	h.upstream.enqueue("t1")
	h.dispatch()
	if got := pod.pollBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 on the next poll, got %v", got)
	}
}

func TestDispatchClaimsForIdlePodAndWakesIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(time.Hour))
	h.upstream.enqueue("t1")
	pod := h.newPod("pool-0")
	pod.start()
	sock := pod.openWake()

	h.dispatch()

	if got := sock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("want a %s frame, got %q", wakeEvent, got)
	}
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want 1 upstream claim, got %d", n)
	}
	if h.upstream.queued() != 0 {
		t.Fatal("task should have left the upstream queue")
	}
	// The task waits on its pod; the lease starts when the pod claims it.
	if inv := h.inventory(); inv.Running != 1 {
		t.Fatalf("want the pod reserved, got %+v", inv)
	}
	if ls := h.leases(); len(ls) != 0 {
		t.Fatalf("no lease before the pod claims, got %+v", ls)
	}
	if got := pod.pollBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 on the pod's claim, got %v", got)
	}
	ls := h.leases()
	if len(ls) != 1 || ls[0].TaskID != "t1" || ls[0].PodID != pod.id || ls[0].EndedAt != nil {
		t.Fatalf("want an active lease for t1, got %+v", ls)
	}
	if status := pod.report("t1", "start"); status != http.StatusOK {
		t.Fatalf("start: status %d", status)
	}
	// One task, one claim: a second round has no free pod and no frame.
	h.upstream.enqueue("t2")
	h.dispatch()
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("leased pod was woken: %q", got)
	}
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want still 1 claim, got %d", n)
	}
}

// A pod without a wake socket is lent too: it picks its task up on its next
// poll.
func TestDispatchServesAPodWithoutSocketOnItsNextPoll(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	h.upstream.enqueue("t1")
	silent := h.newPod("pool-0")
	silent.start()

	h.dispatch()
	if got := silent.pollBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 on the silent pod's poll, got %v", got)
	}
}

func TestDispatchWakesTheTargetPodWhenTaskIsPlacedElsewhere(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	h.upstream.enqueue("t1")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()
	sockA, sockB := a.openWake(), b.openWake()
	if got := a.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 on a, got %v", got)
	}
	if got := sockA.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("pod a: want the frame for t1, got %q", got)
	}

	// a runs t1, so dispatch reserves idle b, claims t2 against it and wakes
	// only b.
	h.upstream.enqueue("t2")
	h.dispatch()

	if got := sockB.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("target pod b: want a frame, got %q", got)
	}
	if got := sockA.next(300 * time.Millisecond); got != "" {
		t.Fatalf("bound pod a was woken for another pod's task: %q", got)
	}
	if got := a.pollBatch(); len(got) != 0 {
		t.Fatalf("a was handed %v", got)
	}
	if got := b.pollBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2 on b, got %v", got)
	}
	got := leasePods(h)
	if got["t1"] != a.id || got["t2"] != b.id {
		t.Fatalf("unexpected placement: %v", got)
	}
}

func TestReconnectReplacesTheOldWakeSocket(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(time.Hour), holding(time.Hour))
	h.upstream.enqueueIssue("t1", "issue-1")
	pod := h.newPod("pool-0")
	pod.start()
	oldSock := pod.openWake()
	newSock := pod.openWake()

	// The manager closes the stale socket.
	_ = oldSock.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := oldSock.conn.ReadMessage(); err == nil {
		t.Fatal("old socket still delivers frames")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("old socket was not closed: %v", err)
	}

	h.dispatch()
	if got := newSock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("new socket: want a frame, got %q", got)
	}
	// The old socket's teardown must not drop the new registration: a further
	// wake still arrives.
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1, got %v", got)
	}
	h.upstream.enqueueIssue("t2", "issue-1")
	pod.report("t1", "complete")
	h.dispatch()
	if got := newSock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("new socket after the old one's teardown: want a frame, got %q", got)
	}
}

func TestRunDispatchesOnItsInterval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(50*time.Millisecond))
	pod := h.newPod("pool-0")
	pod.start()
	sock := pod.openWake()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.pool.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	h.upstream.enqueue("t1")
	if got := sock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("want a frame from Run, got %q", got)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1, got %v", got)
	}
}

// Claims are made only on the dispatch tick, so the tick runs without a
// wake interval too; pods then pick their tasks up by polling.
func TestRunDispatchesWithoutWakeInterval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()
	h.upstream.enqueue("t1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.pool.Run(ctx)
	var got []string
	eventually(t, "the tick to place t1 on the pod", func() bool {
		got = append(got, pod.pollBatch()...)
		return len(got) > 0
	})
	if !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want [t1], got %v", got)
	}
}

// Only the dispatch tick claims: a poll with nothing placed on the pod
// causes no upstream call.
func TestAPollWithNothingPlacedClaimsNothingUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	withSock := h.newPod("pool-0")
	noSock := h.newPod("pool-1")
	withSock.start()
	noSock.start()
	sock := withSock.openWake()

	for _, p := range []*simPod{withSock, noSock} {
		if got := p.pollBatch(); len(got) != 0 {
			t.Fatalf("a poll was handed %v", got)
		}
	}
	if n := h.upstream.claimCount(); n != 0 {
		t.Fatalf("a poll claimed upstream: %d claims", n)
	}
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("a poll caused a hint: %q", got)
	}

	h.dispatch()
	h.dispatch()
	if got := withSock.pollBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("socket pod poll: want [t1], got %v", got)
	}
	if got := noSock.pollBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("socket-less pod poll: want [t2], got %v", got)
	}
	if n := h.upstream.claimCount(); n != 2 {
		t.Fatalf("want 2 claims, got %d", n)
	}
	if inv := h.inventory(); inv.Running != 2 {
		t.Fatalf("want both pods leased, got %+v", inv)
	}
	// Leased pods still cannot claim a second task.
	h.upstream.enqueue("t3")
	if got := withSock.claimBatch(); len(got) != 0 {
		t.Fatalf("leased pod was handed %v", got)
	}
}

const workspacesChangedEvent = "daemon:workspaces_changed"

// syncedWakePod syncs the pool once, registers a pod for A and opens its socket.
func syncedWakePod(t *testing.T, up *syncMultica, ids ...string) (*harness, *wakeSocket) {
	t.Helper()
	h := newSyncHarness(t, up, withWake(time.Hour))
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("pool-0")
	pod.registerFor(ids[0])
	return h, pod.openWake()
}

func TestSyncTellsConnectedPodsAboutAddedWorkspaces(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h, sock := syncedWakePod(t, up, wsA)

	up.setWorkspaces(wsA, wsB)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want %s, got %q", workspacesChangedEvent, got)
	}
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("one change, one frame; got another: %q", got)
	}
}

func TestSyncTellsConnectedPodsAboutRemovedWorkspaces(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h, sock := syncedWakePod(t, up, wsA)

	up.setWorkspaces(wsA)
	// The first sync without B keeps it (nothing changed, nothing to tell); the
	// second removes it.
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("a workspace missing once is kept; got a frame: %q", got)
	}
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want %s, got %q", workspacesChangedEvent, got)
	}
}

func TestSyncSendsNothingWhenTheListIsUnchangedOrUnusable(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h, sock := syncedWakePod(t, up, wsA)

	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	up.setListStatus(http.StatusInternalServerError)
	_, _ = h.pool.Sync(context.Background())
	up.setListStatus(0)
	up.setWorkspaces()
	_, _ = h.pool.Sync(context.Background())
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("nothing changed, yet got %q", got)
	}
}

func TestSyncReachesEveryConnectedPod(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h, first := syncedWakePod(t, up, wsA)
	second := h.newPod("pool-1")
	second.registerFor(wsA)
	secondSock := second.openWake()

	up.setWorkspaces(wsA, wsB)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*wakeSocket{"first": first, "second": secondSock} {
		if got := s.next(5 * time.Second); got != workspacesChangedEvent {
			t.Fatalf("%s pod: want %s, got %q", name, workspacesChangedEvent, got)
		}
	}
}

func TestSyncWithoutSocketsDoesNotBlock(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, withWake(time.Hour))
	pod := h.newPod("pool-0")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.pool.Sync(context.Background()) // no pods at all
		pod.registerFor(wsA)                     // a pod without a socket
		up.setWorkspaces(wsA, wsB)
		_, _ = h.pool.Sync(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Sync blocked without wake sockets")
	}
}

func TestTaskHintsKeepTheirOwnFrameBesideWorkspaceChanges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(time.Hour))
	h.upstream.enqueue("t1")
	pod := h.newPod("pool-0")
	pod.start()
	sock := pod.openWake()

	h.dispatch()
	if got := sock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("a task hint must stay %s, got %q", wakeEvent, got)
	}
}
