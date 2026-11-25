package store

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"strings"
	"testing"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/policy"
)

func mustAEAD(t *testing.T, key byte) cipher.AEAD {
	t.Helper()
	aead, err := newAEAD(bytes.Repeat([]byte{key}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return aead
}

func placed(id, payload string) core.TaskRow {
	return core.TaskRow{
		Task:  core.Task{Task: policy.Task{ID: id}, Payload: json.RawMessage(payload)},
		State: core.TaskPlaced,
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	aead := mustAEAD(t, 1)
	row := placed("task-1", `{"secret":"agent prompt"}`)
	sealed := sealPayload(aead, row)
	if bytes.Contains(sealed, row.Payload) {
		t.Fatalf("the sealed payload holds the payload in clear: %q", sealed)
	}
	got, err := openPayload(aead, "task-1", sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(row.Payload) {
		t.Fatalf("payload = %q, want %q", got, row.Payload)
	}
	if again := sealPayload(aead, row); bytes.Equal(again, sealed) {
		t.Fatal("two encryptions of one payload are equal: the nonce is not fresh")
	}
}

// The column is a 12-byte nonce followed by the AES-256-GCM ciphertext of
// the payload with the task ID as additional data, so any AES-GCM
// implementation reads it.
func TestPayloadColumnFormat(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	row := placed("task-1", `{"a":1}`)
	sealed := sealPayload(mustAEAD(t, 1), row)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != 12+len(row.Payload)+gcm.Overhead() {
		t.Fatalf("sealed length = %d, want %d", len(sealed), 12+len(row.Payload)+gcm.Overhead())
	}
	got, err := gcm.Open(nil, sealed[:12], sealed[12:], []byte("task-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(row.Payload) {
		t.Fatalf("payload = %q, want %q", got, row.Payload)
	}
}

func TestPayloadDoesNotOpenWithAnotherKey(t *testing.T) {
	sealed := sealPayload(mustAEAD(t, 1), placed("task-1", `{"a":1}`))
	if _, err := openPayload(mustAEAD(t, 2), "task-1", sealed); err == nil {
		t.Fatal("a payload opened with another key")
	}
}

// A payload copied to another task's row does not decrypt, and the error
// names that task, not the payload.
func TestPayloadIsBoundToItsTask(t *testing.T) {
	aead := mustAEAD(t, 1)
	sealed := sealPayload(aead, placed("task-1", `{"secret":"agent prompt"}`))
	_, err := openPayload(aead, "task-2", sealed)
	if err == nil {
		t.Fatal("task-1's payload opened as task-2's")
	}
	if !strings.Contains(err.Error(), "task-2") || strings.Contains(err.Error(), "agent prompt") {
		t.Fatalf("error = %q, want it to name task-2 and not show the payload", err)
	}
}

// A task with no payload, or one that runs or ended, has a NULL column,
// and a NULL column is no payload.
func TestPayloadColumnIsNullWithoutAPayloadToKeep(t *testing.T) {
	aead := mustAEAD(t, 1)
	running := placed("task-1", `{"a":1}`)
	running.State = core.TaskRunning
	ended := placed("task-1", `{"a":1}`)
	ended.State = core.TaskEnded
	for name, row := range map[string]core.TaskRow{
		"nil payload":   {Task: core.Task{Task: policy.Task{ID: "task-1"}}, State: core.TaskPlaced},
		"empty payload": {Task: core.Task{Task: policy.Task{ID: "task-1"}, Payload: json.RawMessage{}}, State: core.TaskHanded},
		"running":       running,
		"ended":         ended,
	} {
		if sealed := sealPayload(aead, row); sealed != nil {
			t.Errorf("%s: column = %q, want NULL", name, sealed)
		}
	}
	got, err := openPayload(aead, "task-1", nil)
	if err != nil || got != nil {
		t.Fatalf("openPayload(NULL) = %q, %v; want nil, nil", got, err)
	}
}
