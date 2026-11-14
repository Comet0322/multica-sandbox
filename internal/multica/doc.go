// Package multica holds everything the sandbox manager knows of Multica: the
// server's daemon API as the manager calls it (Client, Conn), the daemon
// protocol as the manager serves it to the stock daemon in each pod, the wire
// types both carry (package daemonwire), how a pod daemon's registration
// becomes the manager's own, and the affinity rules that read what a Multica
// task is about.
//
// It knows nothing of pods, leases or the lending policy's state; the manager
// decides what to call and acts on the answers.
package multica
