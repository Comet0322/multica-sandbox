// Package store keeps the core's dispatch record in Postgres: the pods the
// core holds and the tasks it claimed, so a manager that restarts takes over
// what the previous run left. It implements core.Store.
//
// A task's payload is kept encrypted with AES-256-GCM, the task ID as
// additional data, and only while the task is placed or handed. One that
// does not decrypt (the key changed) is loaded as none. A task's email is
// never kept.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"sandbox-manager/internal/core"
)

// Retention is how long PruneLoop keeps a pod that is gone, and a task that
// ended and whose failure report the source took.
const Retention = 30 * 24 * time.Hour

//go:embed migrations/*.sql
var migrations embed.FS

// Config is what Open needs.
type Config struct {
	// URL is the Postgres connection string.
	URL string
	// PayloadKey is the AES-256 key, 32 bytes, that task payloads are
	// encrypted with.
	PayloadKey []byte
	// Log is the store's logger. Nil means slog.Default().
	Log *slog.Logger
}

// Store is the dispatch record in Postgres.
type Store struct {
	pool *pgxpool.Pool
	aead cipher.AEAD
	log  *slog.Logger
}

var _ core.Store = (*Store)(nil)

// Open connects to the database and brings its schema up to date. Its
// errors never carry the URL's password.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	aead, err := newAEAD(cfg.PayloadKey)
	if err != nil {
		return nil, err
	}
	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx takes the password out of a parse error only as best it can.
		return nil, errors.New("the database URL does not parse")
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	if err := migrate(ctx, pcfg.ConnConfig, cfg.Log); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate the database: %w", err)
	}
	return &Store{pool: pool, aead: aead, log: cfg.Log}, nil
}

// migrate runs the migrations not yet applied, holding an advisory lock so
// that managers starting together take turns. It uses connections of its
// own, closed when it returns, so the lock never outlives it.
func migrate(ctx context.Context, conn *pgx.ConnConfig, log *slog.Logger) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	db := stdlib.OpenDB(*conn.Copy())
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, dir,
		goose.WithTableName("schema_version"),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		return err
	}
	applied, err := p.Up(ctx)
	for _, m := range applied {
		log.Info("database migrated", "version", m.Source.Version, "file", m.Source.Path)
	}
	return err
}

// Close closes the connections.
func (s *Store) Close() {
	s.pool.Close()
}

const upsertPod = `
INSERT INTO pods (id, state, phase, tenant, kind, key, since, until, created_at, ready_at, gone_at, gone_why)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (id) DO UPDATE SET
	state = excluded.state, phase = excluded.phase, tenant = excluded.tenant,
	kind = excluded.kind, key = excluded.key, since = excluded.since,
	until = excluded.until, created_at = excluded.created_at,
	ready_at = excluded.ready_at, gone_at = excluded.gone_at,
	gone_why = excluded.gone_why`

const upsertTask = `
INSERT INTO tasks (id, tenant, kind, key, pod, state, payload, claimed_at, started_at, ended_at, end_reason, fail_reason, fail_pending)
VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (id) DO UPDATE SET
	tenant = excluded.tenant, kind = excluded.kind, key = excluded.key,
	pod = excluded.pod, state = excluded.state, payload = excluded.payload,
	claimed_at = excluded.claimed_at, started_at = excluded.started_at,
	ended_at = excluded.ended_at, end_reason = excluded.end_reason,
	fail_reason = excluded.fail_reason, fail_pending = excluded.fail_pending`

// Write saves the rows in one transaction, each replacing the row of the
// same ID.
func (s *Store) Write(ctx context.Context, pods []core.PodRow, tasks []core.TaskRow) error {
	if len(pods)+len(tasks) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, p := range pods {
		b.Queue(upsertPod, p.ID, podState(p), p.Phase, p.Tenant, p.Kind, p.Key,
			tz{&p.Since}, tz{&p.Until}, tz{&p.CreatedAt}, tz{&p.ReadyAt}, tz{&p.GoneAt}, p.GoneWhy)
	}
	for _, t := range tasks {
		b.Queue(upsertTask, t.ID, t.Tenant, t.Kind, t.Key, t.PodID, t.State, sealPayload(s.aead, t),
			tz{&t.ClaimedAt}, tz{&t.StartedAt}, tz{&t.EndedAt}, t.EndReason, t.FailReason, t.FailPending)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, b).Close()
	})
	if err != nil {
		return fmt.Errorf("write the dispatch record: %w", err)
	}
	return nil
}

// podState is the state column of a pod row: gone once the core let it go.
func podState(p core.PodRow) string {
	if p.GoneAt.IsZero() {
		return "ready"
	}
	return "gone"
}

const loadPods = `
SELECT id, phase, tenant, kind, key, since, until, created_at, ready_at, gone_at, gone_why
FROM pods WHERE state <> 'gone' ORDER BY ready_at, id`

const loadTasks = `
SELECT id, tenant, kind, key, coalesce(pod, ''), state, payload, claimed_at, started_at, ended_at, end_reason, fail_reason, fail_pending
FROM tasks WHERE state <> 'ended' OR fail_pending ORDER BY claimed_at, id`

// Load reads, from one snapshot, the pods that are not gone in the order
// they were acquired, and the tasks that did not end or whose failure report
// is pending in the order they were claimed. A payload that does not
// decrypt is logged and loaded as none.
func (s *Store) Load(ctx context.Context) (core.Record, error) {
	var rec core.Record
	snapshot := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := pgx.BeginTxFunc(ctx, s.pool, snapshot, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, loadPods)
		pods, err := pgx.CollectRows(rows, scanPod)
		if err != nil {
			return err
		}
		rows, _ = tx.Query(ctx, loadTasks)
		tasks, err := pgx.CollectRows(rows, s.scanTask)
		if err != nil {
			return err
		}
		rec = core.Record{Pods: pods, Tasks: tasks}
		return nil
	})
	if err != nil {
		return core.Record{}, fmt.Errorf("load the dispatch record: %w", err)
	}
	return rec, nil
}

func scanPod(row pgx.CollectableRow) (core.PodRow, error) {
	var p core.PodRow
	err := row.Scan(&p.ID, &p.Phase, &p.Tenant, &p.Kind, &p.Key,
		tz{&p.Since}, tz{&p.Until}, tz{&p.CreatedAt}, tz{&p.ReadyAt}, tz{&p.GoneAt}, &p.GoneWhy)
	return p, err
}

func (s *Store) scanTask(row pgx.CollectableRow) (core.TaskRow, error) {
	var t core.TaskRow
	var sealed []byte
	err := row.Scan(&t.ID, &t.Tenant, &t.Kind, &t.Key, &t.PodID, &t.State, &sealed,
		tz{&t.ClaimedAt}, tz{&t.StartedAt}, tz{&t.EndedAt}, &t.EndReason, &t.FailReason, &t.FailPending)
	if err != nil {
		return t, err
	}
	if t.Payload, err = openPayload(s.aead, t.ID, sealed); err != nil {
		s.log.Error("load a task without its payload", "task_id", t.ID, "error", err)
	}
	return t, nil
}

// Prune deletes the pods that went before before, and the tasks that ended
// before before and whose failure report is not pending.
func (s *Store) Prune(ctx context.Context, before time.Time) (pods, tasks int64, err error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM pods WHERE state = 'gone' AND gone_at < $1`, before)
	if err != nil {
		return 0, 0, fmt.Errorf("prune the pods: %w", err)
	}
	pods = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM tasks WHERE state = 'ended' AND NOT fail_pending AND ended_at < $1`, before)
	if err != nil {
		return pods, 0, fmt.Errorf("prune the tasks: %w", err)
	}
	return pods, tag.RowsAffected(), nil
}

// PruneLoop prunes what is older than Retention now and then hourly, until
// ctx ends.
func (s *Store) PruneLoop(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		pods, tasks, err := s.Prune(ctx, time.Now().Add(-Retention))
		if err != nil && ctx.Err() == nil {
			s.log.Error("prune the dispatch record", "error", err)
		}
		if pods+tasks > 0 {
			s.log.Info("dispatch record pruned", "pods", pods, "tasks", tasks)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// tz is a time kept in a nullable timestamptz column: a zero time is NULL,
// and a time read back is in UTC.
type tz struct{ t *time.Time }

func (z tz) TimestamptzValue() (pgtype.Timestamptz, error) {
	return pgtype.Timestamptz{Time: *z.t, Valid: !z.t.IsZero()}, nil
}

func (z tz) ScanTimestamptz(v pgtype.Timestamptz) error {
	*z.t = time.Time{}
	if v.Valid {
		*z.t = v.Time.UTC()
	}
	return nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("the payload key is %d bytes, not 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealPayload is the payload column of a task row: its payload encrypted,
// bound to its ID, while it is placed or handed; NULL once it runs, or when
// it has none.
func sealPayload(aead cipher.AEAD, t core.TaskRow) []byte {
	if len(t.Payload) == 0 || (t.State != core.TaskPlaced && t.State != core.TaskHanded) {
		return nil
	}
	// A fresh random nonce for every seal, stored in front of the ciphertext
	// (the layout cipher.NewGCMWithRandomNonce uses, so earlier rows open).
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(t.Payload)+aead.Overhead())
	_, _ = rand.Read(nonce) // crypto/rand.Read never fails
	return aead.Seal(nonce, nonce, t.Payload, []byte(t.ID))
}

// openPayload decrypts the payload column of the task with the given ID.
func openPayload(aead cipher.AEAD, id string, sealed []byte) (json.RawMessage, error) {
	if sealed == nil {
		return nil, nil
	}
	n := aead.NonceSize()
	if len(sealed) < n {
		return nil, fmt.Errorf("the payload of task %s does not decrypt: %d bytes, shorter than its nonce", id, len(sealed))
	}
	payload, err := aead.Open(nil, sealed[:n], sealed[n:], []byte(id))
	if err != nil {
		return nil, fmt.Errorf("the payload of task %s does not decrypt: %w", id, err)
	}
	return payload, nil
}
