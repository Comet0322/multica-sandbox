package podsupervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const commandReset = "reset"

// Events the supervisor reports (see the package documentation).
const (
	eventStarted      = "started"
	eventDaemonExited = "daemon-exited"
)

// eventAttempts bounds how often an event is sent before the supervisor goes
// on without the server knowing.
const eventAttempts = 3

// errUnknownPod is a control URL answering 404: the server does not know the
// pod.
var errUnknownPod = errors.New("control url does not know the pod")

// errUnknownToken is a control URL answering 401: the manager refuses the pod
// token, which it never forgets while it runs. It restarted (its registry is
// in memory) or pruned the pod, and only a new registration brings the pod
// back.
var errUnknownToken = errors.New("control url refuses the pod token")

// Command is the body the control URL returns to a poll.
type Command struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}

// Report is what the supervisor POSTs back once a command is finished, or, with
// only Event set, an event.
type Report struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"` // "done"
	// Failed lists the paths a reset could not delete.
	Failed []string `json:"failed,omitempty"`
	Event  string   `json:"event,omitempty"`
}

// poll fetches commands until ctx ends, runs a reset itself (it is a file
// operation the daemon keeps running through) and reports it back.
func (s *Supervisor) poll(ctx context.Context) {
	var last *Report // outcome of the most recently handled command
	var unknownSince time.Time
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cmd, err := s.fetchCommand(ctx)
		if errors.Is(err, errUnknownToken) {
			// Exit at once so the container restarts and its entrypoint
			// registers the pod again: after a manager restart the pods come
			// back within a poll interval, not after UnknownTimeout.
			s.fail(errors.New("the pool manager refuses this pod's token (it restarted or dropped the pod); exiting so the container restarts and registers again"))
			return
		}
		if errors.Is(err, errUnknownPod) {
			// A short spell can be a manager that is starting up. For longer,
			// nothing will bring the pod back (the manager dropped it):
			// restart, so the entrypoint registers it again.
			if unknownSince.IsZero() {
				unknownSince = time.Now()
				s.log.Info("the pool manager does not know this pod yet")
			} else if time.Since(unknownSince) > s.cfg.UnknownTimeout {
				s.fail(fmt.Errorf("the pool manager has not known this pod for %s; exiting so the container restarts", s.cfg.UnknownTimeout))
				return
			}
			continue
		}
		if err != nil {
			s.log.Warn("poll control url failed", "error", err)
			continue
		}
		unknownSince = time.Time{}
		if cmd == nil || cmd.Command != commandReset || cmd.ID == "" {
			continue
		}
		if last == nil || last.ID != cmd.ID {
			failed := s.resetDaemonHome()
			s.log.Info("reset done", "id", cmd.ID, "failed", len(failed))
			last = &Report{ID: cmd.ID, Status: "done", Failed: failed}
		}
		if err := s.sendReport(ctx, *last); err != nil {
			s.log.Warn("report reset outcome failed, will retry", "id", last.ID, "error", err)
		}
	}
}

func (s *Supervisor) fetchCommand(ctx context.Context) (*Command, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.ControlURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.PodToken)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errUnknownToken
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errUnknownPod
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control url returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var cmd Command
	if err := json.Unmarshal(body, &cmd); err != nil {
		return nil, fmt.Errorf("decode command: %w", err)
	}
	return &cmd, nil
}

func (s *Supervisor) sendReport(ctx context.Context, r Report) error {
	status, err := s.post(ctx, r)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("control url rejected report: HTTP %d", status)
	}
	return nil
}

// post sends a report and returns the response status.
func (s *Supervisor) post(ctx context.Context, r Report) (int, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.ControlURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.PodToken)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// fail makes Run stop with err. It never blocks.
func (s *Supervisor) fail(err error) {
	select {
	case s.giveUp <- err:
	default:
	}
}

// sendEvent reports an event, trying again only when the server could not be
// reached or failed (5xx). Any other answer counts as delivered: a server that
// does not know the pod (404) holds nothing of it, and one that does not know
// events would refuse them every time.
func (s *Supervisor) sendEvent(ctx context.Context, event string) {
	for i := 0; i < eventAttempts; i++ {
		status, err := s.post(ctx, Report{Event: event})
		if err == nil && status < 500 {
			return
		}
		if err == nil {
			err = fmt.Errorf("control url returned HTTP %d", status)
		}
		s.log.Warn("report event failed", "event", event, "attempt", i+1, "error", err)
		if !sleepCtx(ctx, time.Duration(i+1)*time.Second) {
			return
		}
	}
}

// afterEvent reports the event in the background and fires delay after that
// attempt ended.
func (s *Supervisor) afterEvent(ctx context.Context, event string, delay time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	go func() {
		s.sendEvent(ctx, event)
		if sleepCtx(ctx, delay) {
			ch <- time.Now()
		}
	}()
	return ch
}

// sleepCtx waits for d and reports false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
