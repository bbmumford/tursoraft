package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bbmumford/tursoraft/mesh/meshtest"
)

// runHandshake runs the member proof between a dialer holding clientKeys and
// an acceptor holding serverKeys, with the given applied cluster IDs, and
// returns each side's error.
func runHandshake(t *testing.T, clientKeys, serverKeys [][]byte, clientID, serverID string) (clientErr, serverErr error) {
	t.Helper()
	hub := meshtest.NewHub()
	a, b := hub.Node("n-a", svcAI), hub.Node("n-b", svcAI)
	defer a.Close()
	defer b.Close()
	member := func(keys [][]byte) [][]byte {
		var out [][]byte
		for _, k := range keys {
			mk, err := MemberKey(k, "control/orbtr-control")
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, mk)
		}
		return out
	}
	client := handshake{cluster: "control/orbtr-control", local: "n-a", keys: member(clientKeys), standing: func() standing { return standing{cluster: clientID} }}
	server := handshake{cluster: "control/orbtr-control", local: "n-b", keys: member(serverKeys), standing: func() standing { return standing{cluster: serverID, state: serverID != ""} }}
	ln, err := b.Listen("k")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		err = server.server(c, time.Now().Add(2*time.Second))
		c.Close()
		done <- err
	}()
	c, err := a.Dial(context.Background(), "n-b", "k")
	if err != nil {
		t.Fatal(err)
	}
	_, clientErr = client.client(c, time.Now().Add(2*time.Second))
	c.Close()
	return clientErr, <-done
}

func TestProofWithTheSameKeyPasses(t *testing.T) {
	k := testKey()
	if c, s := runHandshake(t, [][]byte{k}, [][]byte{k}, "", ""); c != nil || s != nil {
		t.Fatalf("client %v, server %v", c, s)
	}
}

func TestProofWithAnotherKeyFailsBothWays(t *testing.T) {
	c, s := runHandshake(t, [][]byte{testKey()}, [][]byte{testKey()}, "", "")
	if !errors.Is(c, ErrProofFailed) {
		t.Errorf("the dialer accepted an acceptor with another key: %v", c)
	}
	if s == nil {
		t.Error("the acceptor accepted a dialer with another key")
	}
}

// During a rotation a member with the new and the prior key interoperates
// with one still on the prior key, whichever side dials.
func TestProofUnderThePriorKeyIsAccepted(t *testing.T) {
	prior, next := testKey(), testKey()
	for _, tc := range []struct {
		name           string
		client, server [][]byte
	}{
		{"rotated dials old", [][]byte{next, prior}, [][]byte{prior}},
		{"old dials rotated", [][]byte{prior}, [][]byte{next, prior}},
		{"rotated dials rotated", [][]byte{next, prior}, [][]byte{next, prior}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c, s := runHandshake(t, tc.client, tc.server, "", ""); c != nil || s != nil {
				t.Fatalf("client %v, server %v", c, s)
			}
		})
	}
	if c, _ := runHandshake(t, [][]byte{next}, [][]byte{prior}, "", ""); !errors.Is(c, ErrProofFailed) {
		t.Errorf("a member holding only the new key passed against one holding only the prior: %v", c)
	}
}

// Members that have applied different cluster IDs refuse each other; a member
// with no ID yet is accepted, because it may be joining.
func TestProofRefusesAForeignClusterID(t *testing.T) {
	k := [][]byte{testKey()}
	c, s := runHandshake(t, k, k, "cluster-a", "cluster-b")
	if !errors.Is(c, ErrForeignCluster) {
		t.Errorf("dialer: got %v, want ErrForeignCluster", c)
	}
	if s == nil {
		t.Error("the acceptor accepted a foreign cluster")
	}
	if c, s := runHandshake(t, k, k, "", "cluster-b"); c != nil || s != nil {
		t.Errorf("a joining member was refused: client %v, server %v", c, s)
	}
}

// A proof binds both NodeIDs and both nonces, so the proof one pair made is
// not the proof another pair needs.
func TestProofBindsThePairAndTheNonces(t *testing.T) {
	mk, _ := MemberKey(testKey(), "control/orbtr-control")
	h := handshake{cluster: "control/orbtr-control", local: "n-a", keys: [][]byte{mk}}
	n1, n2 := make([]byte, 32), make([]byte, 32)
	n2[0] = 1
	base := h.proof(mk, "n-b", n1, n2, "n-a")
	other := handshake{cluster: "control/orbtr-control", local: "n-c", keys: [][]byte{mk}}
	for name, p := range map[string][]byte{
		"another peer":   h.proof(mk, "n-c", n1, n2, "n-a"),
		"another nonce":  h.proof(mk, "n-b", n2, n1, "n-a"),
		"another sender": other.proof(mk, "n-b", n1, n2, "n-c"),
	} {
		if string(p) == string(base) {
			t.Errorf("%s produced the same proof", name)
		}
	}
	a, _ := MemberKey(testKey(), "control/orbtr-control")
	b, _ := MemberKey(testKey(), "sessions/orbtr-sessions")
	if string(a) == string(b) {
		t.Error("two clusters derived the same member key")
	}
}
