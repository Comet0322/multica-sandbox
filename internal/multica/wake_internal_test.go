package multica

import (
	"bytes"
	"testing"
	"time"
)

func TestWakeSendsNeverBlockWhenASocketIsBacklogged(t *testing.T) {
	t.Parallel()
	api := NewDaemonAPI(DaemonAPIConfig{Wake: true})
	// A socket whose writer is stuck: nothing drains send.
	wc := &wakeConn{send: make(chan []byte, wakeSendBuffer)}
	api.wakers["10.0.0.1"] = wc

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 3 * wakeSendBuffer {
			api.notifyWorkspacesChanged()
			api.WakePod("10.0.0.1")
		}
		api.WakePod("10.0.0.9") // no socket at all
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full send buffer blocked the sender")
	}
	if len(wc.send) != wakeSendBuffer {
		t.Fatalf("want a full buffer of %d, got %d", wakeSendBuffer, len(wc.send))
	}
	// The first hints stay queued, the overflow is dropped.
	if got := <-wc.send; !bytes.Equal(got, workspacesChangedFrame) {
		t.Fatalf("first queued frame = %s", got)
	}
}

func TestFramesAreDistinct(t *testing.T) {
	t.Parallel()
	if bytes.Equal(wakeFrame, workspacesChangedFrame) {
		t.Fatal("the two hints must differ")
	}
	if !bytes.Contains(workspacesChangedFrame, []byte("daemon:workspaces_changed")) || !bytes.Contains(wakeFrame, []byte("daemon:task_available")) {
		t.Fatalf("unexpected frames: %s / %s", wakeFrame, workspacesChangedFrame)
	}
}

// The workspaces_changed hint has a slot of its own, so wake hints filling
// the send buffer cannot crowd it out, and repeated hints coalesce.
func TestWorkspacesChangedIsNotCrowdedOutByWakes(t *testing.T) {
	t.Parallel()
	api := NewDaemonAPI(DaemonAPIConfig{Wake: true})
	wc := &wakeConn{send: make(chan []byte, wakeSendBuffer), changed: make(chan struct{}, 1)}
	for range wakeSendBuffer {
		wc.send <- wakeFrame // the buffer is full of wakes already
	}
	api.wakers["10.0.0.1"] = wc

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 {
			api.notifyWorkspacesChanged()
			api.WakePod("10.0.0.1")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full wake buffer blocked the hint")
	}
	if len(wc.changed) != 1 {
		t.Fatalf("five hints must coalesce into one pending signal, got %d", len(wc.changed))
	}
	if len(wc.send) != wakeSendBuffer {
		t.Fatalf("the wake buffer is untouched by the hint: %d", len(wc.send))
	}
	for range wakeSendBuffer {
		if got := <-wc.send; !bytes.Equal(got, wakeFrame) {
			t.Fatalf("the send buffer holds only wakes, got %s", got)
		}
	}
}

// A socket made without the signal still gets the hint, through the wake
// buffer.
func TestWorkspacesChangedReachesASocketWithoutTheSignal(t *testing.T) {
	t.Parallel()
	api := NewDaemonAPI(DaemonAPIConfig{Wake: true})
	wc := &wakeConn{send: make(chan []byte, wakeSendBuffer)}
	api.wakers["10.0.0.1"] = wc
	api.notifyWorkspacesChanged()
	select {
	case got := <-wc.send:
		if !bytes.Equal(got, workspacesChangedFrame) {
			t.Fatalf("got %s", got)
		}
	default:
		t.Fatal("no hint was queued")
	}
}
