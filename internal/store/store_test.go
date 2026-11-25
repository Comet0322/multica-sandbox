package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/policy"
	"sandbox-manager/internal/store"
)

var key = bytes.Repeat([]byte{7}, 32)

func TestOpenRefusesAKeyThatIsNot32Bytes(t *testing.T) {
	for _, n := range []int{0, 16, 24, 33} {
		_, err := store.Open(t.Context(), store.Config{URL: "postgres://u@127.0.0.1:1/db", PayloadKey: make([]byte, n)})
		if err == nil {
			t.Errorf("Open took a %d-byte key", n)
		}
	}
}

func TestOpenErrorsDoNotCarryThePassword(t *testing.T) {
	marker := "hunter" + "2-s3cret"
	for name, u := range map[string]string{
		"unreachable":    "postgres://user:" + marker + "@127.0.0.1:1/db?connect_timeout=5",
		"bad port":       "postgres://user:" + marker + "@127.0.0.1:port/db",
		"bad userinfo":   "postgres://user:" + marker + "@x@127.0.0.1:5432/db%zz",
		"key value":      "host=127.0.0.1 port=nope user=user password=" + marker,
		"quoted":         "host=127.0.0.1 port=nope user=user password='" + marker + " x'",
		"unknown scheme": "mysql://user:" + marker + "@127.0.0.1:1/db",
	} {
		_, err := store.Open(t.Context(), store.Config{URL: u, PayloadKey: key})
		if err == nil {
			t.Errorf("%s: Open succeeded", name)
		} else if strings.Contains(err.Error(), marker) {
			t.Errorf("%s: the error carries the password: %v", name, err)
		}
	}
}

// newSchema makes a schema of the test's own, dropped when the test ends,
// and returns a URL that selects it and a connection that reads it.
func newSchema(t *testing.T) (string, *pgx.Conn) {
	t.Helper()
	base := os.Getenv("SANDBOX_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SANDBOX_TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("SANDBOX_TEST_DATABASE_URL must be a postgres:// URL: %v", err)
	}
	schema := "store_test_" + strings.ToLower(rand.Text()[:12])
	conn, err := pgx.Connect(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		conn.Close(ctx)
	})
	if _, err := conn.Exec(t.Context(), "SET search_path TO "+schema); err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), conn
}

func open(t *testing.T, u string) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), store.Config{URL: u, PayloadKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// at is a time on the test's day, to the microsecond Postgres keeps.
func at(minute int) time.Time {
	return time.Date(2026, 11, 26, 9, 0, 0, 123456000, time.UTC).Add(time.Duration(minute) * time.Minute)
}

var cst = time.FixedZone("CST", 8*60*60)

func task(id string, state core.TaskState, claimed time.Time) core.TaskRow {
	return core.TaskRow{
		Task:      core.Task{Task: policy.Task{ID: id, Tenant: "ws-1", Kind: policy.KindPool}},
		State:     state,
		ClaimedAt: claimed,
	}
}

func write(t *testing.T, s *store.Store, pods []core.PodRow, tasks []core.TaskRow) {
	t.Helper()
	if err := s.Write(t.Context(), pods, tasks); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, s *store.Store) core.Record {
	t.Helper()
	rec, err := s.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func podIDs(rec core.Record) []string {
	ids := []string{}
	for _, p := range rec.Pods {
		ids = append(ids, p.ID)
	}
	return ids
}

func taskIDs(rec core.Record) []string {
	ids := []string{}
	for _, t := range rec.Tasks {
		ids = append(ids, t.ID)
	}
	return ids
}

// column reads one value of the raw tables.
func column[T any](t *testing.T, conn *pgx.Conn, query string, args ...any) T {
	t.Helper()
	var v T
	if err := conn.QueryRow(t.Context(), query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

func ids(t *testing.T, conn *pgx.Conn, query string) []string {
	t.Helper()
	rows, _ := conn.Query(t.Context(), query)
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return got
}

// Managers that start together, and one that starts later, all open the
// store; one of them migrates and logs it, the schema is the migration's and
// goose keeps its version in schema_version.
func TestOpenMigrates(t *testing.T) {
	u, conn := newSchema(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			s, err := store.Open(t.Context(), store.Config{URL: u, PayloadKey: key, Log: log})
			if err == nil {
				s.Close()
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Open together: %v", err)
		}
	}
	open(t, u)

	if n := strings.Count(logs.String(), `msg="database migrated" version=1 `); n != 1 {
		t.Errorf("the migration was logged %d times, want once:\n%s", n, logs.String())
	}
	tables := ids(t, conn, `SELECT table_name::text FROM information_schema.tables WHERE table_schema = current_schema() ORDER BY 1`)
	if want := []string{"pods", "schema_version", "tasks"}; !reflect.DeepEqual(tables, want) {
		t.Fatalf("tables = %v, want %v", tables, want)
	}
	if v := column[int64](t, conn, `SELECT max(version_id) FROM schema_version`); v != 1 {
		t.Fatalf("schema version = %d, want 1", v)
	}
}

func TestWriteLoadRoundTrip(t *testing.T) {
	u, conn := newSchema(t)
	s := open(t, u)
	pods := []core.PodRow{
		{ID: "pod-a", Phase: policy.PhaseRunning, Tenant: "ws-1", Kind: policy.KindClean, Key: "issue-1",
			Since: at(2).In(cst), Until: at(9).In(cst), CreatedAt: at(0).In(cst), ReadyAt: at(1).In(cst)},
		{ID: "pod-b", Phase: policy.PhaseWarm, ReadyAt: at(3)},
		{ID: "pod-c", Phase: policy.PhaseIdle, Tenant: "ws-1", Kind: policy.KindPool, ReadyAt: at(0),
			GoneAt: at(8).In(cst), GoneWhy: "handed back"},
	}
	handed := core.TaskRow{
		Task: core.Task{
			Task:    policy.Task{ID: "task-a", Tenant: "ws-1", Kind: policy.KindClean, Key: "issue-1"},
			Email:   "person@example.com",
			Payload: json.RawMessage(`{"task":"a","prompt":"do it"}`),
		},
		PodID: "pod-a", State: core.TaskHanded, ClaimedAt: at(1).In(cst), StartedAt: at(2).In(cst),
	}
	waiting := core.TaskRow{
		Task:      core.Task{Task: policy.Task{ID: "task-b", Tenant: "ws-2", Kind: policy.KindPool}, Payload: json.RawMessage(`{"task":"b"}`)},
		State:     core.TaskPlaced,
		ClaimedAt: at(4),
	}
	failed := core.TaskRow{
		Task: core.Task{
			Task:    policy.Task{ID: "task-c", Tenant: "ws-1", Kind: policy.KindPool, Key: "chat-1"},
			Email:   "person@example.com",
			Payload: json.RawMessage(`{"task":"c"}`),
		},
		PodID: "pod-c", State: core.TaskEnded, ClaimedAt: at(5), StartedAt: at(6), EndedAt: at(7).In(cst),
		EndReason: "pod lost", FailReason: "the pod was lost", FailPending: true,
	}
	write(t, s, pods, []core.TaskRow{handed, waiting, failed})

	got := load(t, s)
	wantPods := []core.PodRow{
		{ID: "pod-a", Phase: policy.PhaseRunning, Tenant: "ws-1", Kind: policy.KindClean, Key: "issue-1",
			Since: at(2), Until: at(9), CreatedAt: at(0), ReadyAt: at(1)},
		{ID: "pod-b", Phase: policy.PhaseWarm, ReadyAt: at(3)},
	}
	if !reflect.DeepEqual(got.Pods, wantPods) {
		t.Errorf("pods =\n%+v\nwant\n%+v", got.Pods, wantPods)
	}
	handed.Email, handed.ClaimedAt, handed.StartedAt = "", at(1), at(2)
	failed.Email, failed.Payload, failed.EndedAt = "", nil, at(7)
	if want := []core.TaskRow{handed, waiting, failed}; !reflect.DeepEqual(got.Tasks, want) {
		t.Errorf("tasks =\n%+v\nwant\n%+v", got.Tasks, want)
	}

	if column[string](t, conn, `SELECT state FROM pods WHERE id = 'pod-c'`) != "gone" ||
		!column[time.Time](t, conn, `SELECT gone_at FROM pods WHERE id = 'pod-c'`).Equal(at(8)) ||
		column[string](t, conn, `SELECT gone_why FROM pods WHERE id = 'pod-c'`) != "handed back" {
		t.Error("pod-c is not kept as gone at 9:08 for being handed back")
	}
	if !column[bool](t, conn, `SELECT since IS NULL AND until IS NULL AND created_at IS NULL FROM pods WHERE id = 'pod-b'`) {
		t.Error("pod-b's zero times are not NULL")
	}
	if !column[bool](t, conn, `SELECT pod IS NULL AND started_at IS NULL AND ended_at IS NULL FROM tasks WHERE id = 'task-b'`) {
		t.Error("task-b's empty pod and zero times are not NULL")
	}
	sealed := column[[]byte](t, conn, `SELECT payload FROM tasks WHERE id = 'task-a'`)
	if bytes.Contains(sealed, []byte("do it")) || len(sealed) != 12+len(handed.Payload)+16 {
		t.Errorf("task-a's payload column = %q, want it encrypted", sealed)
	}
	if rows := column[string](t, conn, `SELECT string_agg(t::text, ' ') FROM tasks t`); strings.Contains(rows, "person@example.com") {
		t.Errorf("a task row keeps the email: %s", rows)
	}
}

func TestWriteReplacesTheRowOfTheSameID(t *testing.T) {
	u, conn := newSchema(t)
	s := open(t, u)
	first := task("task-1", core.TaskPlaced, at(0))
	first.PodID, first.Payload = "pod-1", json.RawMessage(`{"a":1}`)
	pod := core.PodRow{ID: "pod-1", Phase: policy.PhaseWarm, ReadyAt: at(0)}
	write(t, s, []core.PodRow{pod}, []core.TaskRow{first})

	second := task("task-1", core.TaskRunning, at(0))
	second.PodID, second.Payload, second.StartedAt = "pod-2", json.RawMessage(`{"a":1}`), at(1)
	gone := pod
	gone.GoneAt, gone.GoneWhy = at(2), "lost"
	for range 2 {
		write(t, s, []core.PodRow{gone}, []core.TaskRow{second})
	}

	got := load(t, s)
	second.Payload = nil
	if len(got.Pods) != 0 || !reflect.DeepEqual(got.Tasks, []core.TaskRow{second}) {
		t.Fatalf("record = %+v, want no pod and only %+v", got, second)
	}
	if !column[bool](t, conn, `SELECT payload IS NULL FROM tasks WHERE id = 'task-1'`) {
		t.Fatal("the running task keeps its payload")
	}
	if n := column[int64](t, conn, `SELECT count(*) FROM pods`); n != 1 {
		t.Fatalf("%d pod rows, want 1", n)
	}
}

func TestLoadFiltersAndOrders(t *testing.T) {
	u, _ := newSchema(t)
	s := open(t, u)
	pods := []core.PodRow{
		{ID: "pod-2", Phase: policy.PhaseWarm, ReadyAt: at(2)},
		{ID: "pod-gone", Phase: policy.PhaseWarm, ReadyAt: at(0), GoneAt: at(5)},
		{ID: "pod-1", Phase: policy.PhaseWarm, ReadyAt: at(2)},
		{ID: "pod-3", Phase: policy.PhaseWarm, ReadyAt: at(1)},
	}
	failed := task("task-e", core.TaskEnded, at(2))
	failed.EndedAt, failed.FailReason, failed.FailPending = at(3), "lost", true
	ended := task("task-d", core.TaskEnded, at(0))
	ended.EndedAt, ended.FailReason = at(3), "lost"
	tasks := []core.TaskRow{
		task("task-a", core.TaskPlaced, at(3)),
		failed,
		task("task-c", core.TaskRunning, at(2)),
		ended,
		task("task-b", core.TaskHanded, at(1)),
	}
	write(t, s, pods, tasks)

	got := load(t, s)
	if want := []string{"pod-3", "pod-1", "pod-2"}; !reflect.DeepEqual(podIDs(got), want) {
		t.Errorf("pods = %v, want %v", podIDs(got), want)
	}
	if want := []string{"task-b", "task-c", "task-e", "task-a"}; !reflect.DeepEqual(taskIDs(got), want) {
		t.Errorf("tasks = %v, want %v", taskIDs(got), want)
	}
}

// A task's payload is kept while it is placed or handed, and dropped once
// it runs or ends.
func TestPayloadIsDroppedOnceTheTaskRuns(t *testing.T) {
	u, conn := newSchema(t)
	s := open(t, u)
	payload := json.RawMessage(`{"a":1}`)
	var tasks []core.TaskRow
	for _, state := range []core.TaskState{core.TaskPlaced, core.TaskHanded, core.TaskRunning, core.TaskEnded} {
		r := task(string(state), state, at(0))
		r.Payload, r.FailPending = payload, true
		tasks = append(tasks, r)
	}
	write(t, s, nil, tasks)

	kept := ids(t, conn, `SELECT id FROM tasks WHERE payload IS NOT NULL ORDER BY id`)
	if want := []string{"handed", "placed"}; !reflect.DeepEqual(kept, want) {
		t.Errorf("payload kept for %v, want %v", kept, want)
	}
	got := load(t, s).Tasks
	if len(got) != 4 {
		t.Fatalf("loaded %d tasks, want 4", len(got))
	}
	for _, r := range got {
		var want json.RawMessage
		if r.State == core.TaskPlaced || r.State == core.TaskHanded {
			want = payload
		}
		if !reflect.DeepEqual(r.Payload, want) {
			t.Errorf("%s task loaded with payload %q, want %q", r.State, r.Payload, want)
		}
	}
}

// A payload that does not decrypt (the key changed, or the column was
// tampered with) is loaded as none, and logged by task without the payload;
// the other rows load as they are.
func TestLoadDropsAPayloadThatDoesNotDecrypt(t *testing.T) {
	u, conn := newSchema(t)
	var logged bytes.Buffer
	s, err := store.Open(t.Context(), store.Config{URL: u, PayloadKey: key, Log: slog.New(slog.NewTextHandler(&logged, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	a := task("task-a", core.TaskPlaced, at(0))
	a.Payload = json.RawMessage(`{"secret":"prompt"}`)
	b := task("task-b", core.TaskPlaced, at(1))
	b.Payload = json.RawMessage(`{"secret":"other prompt"}`)
	write(t, s, nil, []core.TaskRow{a, b})
	if _, err := conn.Exec(t.Context(), `UPDATE tasks SET payload = (SELECT payload FROM tasks WHERE id = 'task-a') WHERE id = 'task-b'`); err != nil {
		t.Fatal(err)
	}

	got := load(t, s).Tasks
	if len(got) != 2 || string(got[0].Payload) != string(a.Payload) || got[1].ID != "task-b" || got[1].Payload != nil {
		t.Fatalf("loaded %+v, want task-a with its payload and task-b with none", got)
	}
	if l := logged.String(); !strings.Contains(l, "task-b") || strings.Contains(l, "prompt") {
		t.Fatalf("log = %q, want it to name task-b and not show a payload", l)
	}
}

// A Write with one row the database refuses writes none of its rows.
func TestWriteIsAllOrNone(t *testing.T) {
	u, conn := newSchema(t)
	s := open(t, u)
	pod := core.PodRow{ID: "pod-1", Phase: policy.PhaseWarm, ReadyAt: at(0)}
	placed := task("task-1", core.TaskPlaced, at(0))
	write(t, s, []core.PodRow{pod}, []core.TaskRow{placed})

	running := pod
	running.Phase = policy.PhaseRunning
	handed := placed
	handed.State, handed.PodID = core.TaskHanded, "pod-1"
	err := s.Write(t.Context(),
		[]core.PodRow{running, {ID: "pod-2", Phase: policy.PhaseWarm, ReadyAt: at(1)}},
		[]core.TaskRow{handed, task("task-2", "lost", at(1))})
	if err == nil {
		t.Fatal("Write took a task in an unknown state")
	}

	got := load(t, s)
	if !reflect.DeepEqual(got.Pods, []core.PodRow{pod}) || !reflect.DeepEqual(got.Tasks, []core.TaskRow{placed}) {
		t.Fatalf("record = %+v, want the first write's", got)
	}
	if n := column[int64](t, conn, `SELECT (SELECT count(*) FROM pods) + (SELECT count(*) FROM tasks)`); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
}

func TestPrune(t *testing.T) {
	u, conn := newSchema(t)
	s := open(t, u)
	cutoff := at(30)
	pods := []core.PodRow{
		{ID: "gone-old", Phase: policy.PhaseIdle, ReadyAt: at(0), GoneAt: at(10)},
		{ID: "gone-new", Phase: policy.PhaseIdle, ReadyAt: at(0), GoneAt: at(40)},
		{ID: "held-old", Phase: policy.PhaseHeld, ReadyAt: at(0)},
	}
	endedOld := task("ended-old", core.TaskEnded, at(0))
	endedOld.EndedAt = at(10)
	pending := task("ended-old-pending", core.TaskEnded, at(0))
	pending.EndedAt, pending.FailPending = at(10), true
	endedNew := task("ended-new", core.TaskEnded, at(0))
	endedNew.EndedAt = at(40)
	write(t, s, pods, []core.TaskRow{endedOld, pending, endedNew, task("running-old", core.TaskRunning, at(0))})

	p, n, err := s.Prune(t.Context(), cutoff)
	if err != nil || p != 1 || n != 1 {
		t.Fatalf("Prune = %d pods, %d tasks, %v; want 1, 1, nil", p, n, err)
	}
	if got, want := ids(t, conn, `SELECT id FROM pods ORDER BY id`), []string{"gone-new", "held-old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pods left = %v, want %v", got, want)
	}
	if got, want := ids(t, conn, `SELECT id FROM tasks ORDER BY id`), []string{"ended-new", "ended-old-pending", "running-old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tasks left = %v, want %v", got, want)
	}
	if p, n, err := s.Prune(t.Context(), cutoff); err != nil || p != 0 || n != 0 {
		t.Fatalf("Prune again = %d pods, %d tasks, %v; want 0, 0, nil", p, n, err)
	}
}

// PruneLoop prunes as soon as it starts, logs what it deleted, and returns
// when its context ends.
func TestPruneLoop(t *testing.T) {
	u, conn := newSchema(t)
	var logs bytes.Buffer
	s, err := store.Open(t.Context(), store.Config{URL: u, PayloadKey: key, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	write(t, s, []core.PodRow{
		{ID: "gone-old", Phase: policy.PhaseIdle, ReadyAt: now, GoneAt: now.Add(-store.Retention - time.Hour)},
		{ID: "gone-new", Phase: policy.PhaseIdle, ReadyAt: now, GoneAt: now.Add(-store.Retention + time.Hour)},
	}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.PruneLoop(ctx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for column[int64](t, conn, `SELECT count(*) FROM pods`) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("PruneLoop did not prune the pod gone before the retention")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PruneLoop did not return when its context ended")
	}
	if got := ids(t, conn, `SELECT id FROM pods`); !reflect.DeepEqual(got, []string{"gone-new"}) {
		t.Errorf("pods left = %v, want [gone-new]", got)
	}
	if want := fmt.Sprintf("msg=%q pods=1 tasks=0", "dispatch record pruned"); !strings.Contains(logs.String(), want) {
		t.Errorf("log = %q, want it to hold %q", logs.String(), want)
	}
}
