package raft

import (
	"os"
	"strings"
)

// discoverPeers reads a comma-separated list of peer addresses from an environment variable.
// Returns an empty slice if the environment variable is not set or empty.
// Peer addresses should be in the format "host:port" (e.g., "192.168.1.10:8300,192.168.1.11:8300").
func discoverPeers(envVar string) []string {
	if envVar == "" {
		return []string{}
	}

	peerString := os.Getenv(envVar)
	if peerString == "" {
		return []string{}
	}

	// Split by comma and clean up whitespace
	rawPeers := strings.Split(peerString, ",")
	peers := make([]string, 0, len(rawPeers))

	for _, peer := range rawPeers {
		peer = strings.TrimSpace(peer)
		if peer != "" {
			peers = append(peers, peer)
		}
	}

	return peers
}

// validatePeerAddress performs basic validation on a peer address
func ValidatePeerAddress(addr string) bool {
	if addr == "" {
		return false
	}

	// Basic check for host:port format
	parts := strings.Split(addr, ":")
	if len(parts) != 2 {
		return false
	}

	host := strings.TrimSpace(parts[0])
	port := strings.TrimSpace(parts[1])

	if host == "" || port == "" {
		return false
	}

	return true
}

// filterValidPeers filters out invalid peer addresses
func filterValidPeers(peers []string) []string {
	validPeers := make([]string, 0, len(peers))

	for _, peer := range peers {
		if ValidatePeerAddress(peer) {
			validPeers = append(validPeers, peer)
		}
	}

	return validPeers
}

// DiscoverAndValidatePeers discovers peers from environment and validates them
func DiscoverAndValidatePeers(envVar string) []string {
	peers := discoverPeers(envVar)
	return filterValidPeers(peers)
}

// RemoveSelfFromPeers removes the current node's address from the peer list
func RemoveSelfFromPeers(peers []string, selfAddr string) []string {
	filteredPeers := make([]string, 0, len(peers))

	for _, peer := range peers {
		if peer != selfAddr {
			filteredPeers = append(filteredPeers, peer)
		}
	}

	return filteredPeers
}

// normalizePeerAddress ensures peer address is in a consistent format
func NormalizePeerAddress(addr string) string {
	addr = strings.TrimSpace(addr)

	// If no port is specified, assume default Raft port
	if !strings.Contains(addr, ":") {
		addr = addr + ":8300"
	}

	return addr
}

// NormalizePeerAddresses normalizes a slice of peer addresses
func NormalizePeerAddresses(peers []string) []string {
	normalized := make([]string, len(peers))
	for i, peer := range peers {
		normalized[i] = NormalizePeerAddress(peer)
	}
	return normalized
}
