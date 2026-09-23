package cluster

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bbmumford/tursoraft/mesh"
)

// Handshake errors.
var (
	// ErrProofFailed means the peer could not prove it holds the group key.
	ErrProofFailed = errors.New("tursoraft/cluster: member proof failed")
	// ErrForeignCluster means the peer belongs to a different Raft cluster
	// for the same database, one formed from other state.
	ErrForeignCluster = errors.New("tursoraft/cluster: peer belongs to another cluster")
)

const (
	proofLabel     = "tursoraft-member-v1"
	memberKeyInfo  = "tursoraft/member/v1"
	nonceLen       = 32
	maxClusterIDSz = 255
)

// MemberKey derives the key a cluster's member proof is made under:
// HKDF-SHA256 over the group key, salted with the cluster name, so the proof
// for one database's cluster cannot be replayed on another's.
func MemberKey(groupKey []byte, cluster string) ([]byte, error) {
	return hkdf.Key(sha256.New, groupKey, []byte(cluster), memberKeyInfo, 32)
}

// handshake is the member proof and cluster-ID exchange every Raft stream of
// a cluster opens with.
//
// It exists because the mesh authenticates the peer's node, not its right to
// join this cluster: any process holding the mesh network key can dial. The
// proof shows the peer also holds the group key, which only endpoints that
// sync the group are given. Both NodeIDs come from the mesh (Conn.RemoteID),
// and both nonces are fresh, so a proof recorded on one stream verifies on no
// other.
//
// Each side sends one proof per key it holds, the current key first, and
// accepts the peer when any proof verifies under any of its own keys. During
// a group-key rotation a member configured with the new key and the prior one
// therefore interoperates, in both directions, with a member still holding
// only the prior key.
type handshake struct {
	cluster  string          // "<group>/<db>"
	local    string          // this process's NodeID
	keys     [][]byte        // member keys, the current one first
	standing func() standing // this process's place in the cluster
}

// standing is what a member reports about itself at the end of the
// handshake: the cluster ID it has applied, "" before one, and whether it
// holds any Raft state. A member with state but no applied ID belongs to some
// cluster whose ID it has not yet applied, so a leader must not add it.
type standing struct {
	cluster string
	state   bool
}

func (s standing) bytes() []byte {
	id := s.cluster
	if len(id) > maxClusterIDSz {
		id = id[:maxClusterIDSz]
	}
	var flags byte
	if s.state {
		flags = 1
	}
	return append(append([]byte{byte(len(id))}, id...), flags)
}

func readStanding(r io.Reader) (standing, error) {
	var n [1]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return standing{}, err
	}
	buf := make([]byte, int(n[0])+1)
	if _, err := io.ReadFull(r, buf); err != nil {
		return standing{}, err
	}
	return standing{cluster: string(buf[:n[0]]), state: buf[n[0]]&1 == 1}, nil
}

// maxProofs bounds how many proofs a peer may send: a current and a prior key.
const maxProofs = 2

// proof is HMAC-SHA256(key, label ‖ cluster ‖ low ID ‖ high ID ‖ low nonce ‖
// high nonce ‖ sender), each variable-length field prefixed with its two-byte
// length so no two field lists encode to the same bytes.
func (h handshake) proof(key []byte, remote string, localNonce, remoteNonce []byte, sender string) []byte {
	lowID, highID := h.local, remote
	lowNonce, highNonce := localNonce, remoteNonce
	if remote < h.local {
		lowID, highID = remote, h.local
		lowNonce, highNonce = remoteNonce, localNonce
	}
	mac := hmac.New(sha256.New, key)
	for _, f := range [][]byte{[]byte(proofLabel), []byte(h.cluster), []byte(lowID), []byte(highID), lowNonce, highNonce, []byte(sender)} {
		var n [2]byte
		binary.BigEndian.PutUint16(n[:], uint16(len(f)))
		mac.Write(n[:])
		mac.Write(f)
	}
	return mac.Sum(nil)
}

// proofs is this side's proof message: a count, then one proof per key.
func (h handshake) proofs(remote string, localNonce, remoteNonce []byte) []byte {
	out := []byte{byte(len(h.keys))}
	for _, k := range h.keys {
		out = append(out, h.proof(k, remote, localNonce, remoteNonce, h.local)...)
	}
	return out
}

// readProofs reads the peer's proof message.
func readProofs(r io.Reader) ([][]byte, error) {
	var n [1]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	if n[0] == 0 || n[0] > maxProofs {
		return nil, fmt.Errorf("%w: peer sent %d proofs", ErrProofFailed, n[0])
	}
	out := make([][]byte, n[0])
	for i := range out {
		out[i] = make([]byte, sha256.Size)
		if _, err := io.ReadFull(r, out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// verify reports whether any of the remote's proofs verifies under any of
// this side's keys.
func (h handshake) verify(got [][]byte, remote string, localNonce, remoteNonce []byte) bool {
	for _, k := range h.keys {
		want := h.proof(k, remote, localNonce, remoteNonce, remote)
		for _, g := range got {
			if hmac.Equal(g, want) {
				return true
			}
		}
	}
	return false
}

// checkCluster refuses a peer that has applied a different cluster ID.
func (h handshake) checkCluster(remote, remoteCluster string) error {
	local := h.standing().cluster
	if local != "" && remoteCluster != "" && local != remoteCluster {
		return fmt.Errorf("%w: %s is in %s, this node in %s", ErrForeignCluster, remote, remoteCluster, local)
	}
	return nil
}

// client runs the dialer's side: send a nonce; read the acceptor's nonce,
// proofs and standing; check them; send our proofs and standing. It returns
// the acceptor's standing.
func (h handshake) client(c mesh.Conn, deadline time.Time) (standing, error) {
	remote := c.RemoteID()
	if remote == "" || remote == h.local {
		return standing{}, fmt.Errorf("%w: unusable remote id %q", ErrProofFailed, remote)
	}
	c.SetDeadline(deadline)
	defer c.SetDeadline(time.Time{})
	mine := make([]byte, nonceLen)
	rand.Read(mine)
	if _, err := c.Write(mine); err != nil {
		return standing{}, err
	}
	theirs := make([]byte, nonceLen)
	if _, err := io.ReadFull(c, theirs); err != nil {
		return standing{}, err
	}
	got, err := readProofs(c)
	if err != nil {
		return standing{}, err
	}
	peer, err := readStanding(c)
	if err != nil {
		return standing{}, err
	}
	if !h.verify(got, remote, mine, theirs) {
		return standing{}, fmt.Errorf("%w: from %s", ErrProofFailed, remote)
	}
	if err := h.checkCluster(remote, peer.cluster); err != nil {
		return standing{}, err
	}
	msg := append(h.proofs(remote, mine, theirs), h.standing().bytes()...)
	_, err = c.Write(msg)
	return peer, err
}

// server runs the acceptor's side, mirroring client.
func (h handshake) server(c mesh.Conn, deadline time.Time) error {
	remote := c.RemoteID()
	if remote == "" || remote == h.local {
		return fmt.Errorf("%w: unusable remote id %q", ErrProofFailed, remote)
	}
	c.SetDeadline(deadline)
	defer c.SetDeadline(time.Time{})
	theirs := make([]byte, nonceLen)
	if _, err := io.ReadFull(c, theirs); err != nil {
		return err
	}
	mine := make([]byte, nonceLen)
	rand.Read(mine)
	msg := append(append(mine, h.proofs(remote, mine, theirs)...), h.standing().bytes()...)
	if _, err := c.Write(msg); err != nil {
		return err
	}
	got, err := readProofs(c)
	if err != nil {
		return err
	}
	peer, err := readStanding(c)
	if err != nil {
		return err
	}
	if !h.verify(got, remote, mine, theirs) {
		return fmt.Errorf("%w: from %s", ErrProofFailed, remote)
	}
	return h.checkCluster(remote, peer.cluster)
}
