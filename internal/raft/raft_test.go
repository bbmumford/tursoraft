package raft

import (
	"os"
	"testing"
)

func TestResolveAdvertiseAddress_UsesExplicitHost(t *testing.T) {
	t.Setenv("TURSORAFT_ADVERTISE_HOST", "")
	t.Setenv("RAFT_ADVERTISE_HOST", "")
	t.Setenv("FLY_PRIVATE_IP", "")

	addr, err := ResolveAdvertiseAddress("127.0.0.1:7331")
	if err != nil {
		t.Fatalf("ResolveAdvertiseAddress returned error: %v", err)
	}
	if addr != "127.0.0.1:7331" {
		t.Fatalf("expected explicit advertise address to be preserved, got %q", addr)
	}
}

func TestResolveAdvertiseAddress_UsesFlyPrivateIPForWildcardBind(t *testing.T) {
	t.Setenv("TURSORAFT_ADVERTISE_HOST", "")
	t.Setenv("RAFT_ADVERTISE_HOST", "")
	t.Setenv("FLY_PRIVATE_IP", "fdaa::1234")

	addr, err := ResolveAdvertiseAddress(":7331")
	if err != nil {
		t.Fatalf("ResolveAdvertiseAddress returned error: %v", err)
	}
	if addr != "[fdaa::1234]:7331" {
		t.Fatalf("expected Fly private IP advertise address, got %q", addr)
	}
}

func TestResolveAdvertiseAddress_UsesOverrideBeforeFlyPrivateIP(t *testing.T) {
	t.Setenv("TURSORAFT_ADVERTISE_HOST", "127.0.0.2")
	t.Setenv("RAFT_ADVERTISE_HOST", "")
	t.Setenv("FLY_PRIVATE_IP", "fdaa::1234")

	addr, err := ResolveAdvertiseAddress("[::]:7331")
	if err != nil {
		t.Fatalf("ResolveAdvertiseAddress returned error: %v", err)
	}
	if addr != "127.0.0.2:7331" {
		t.Fatalf("expected explicit advertise override, got %q", addr)
	}
}

func TestResolveAdvertiseHost_FallbackDoesNotError(t *testing.T) {
	for _, key := range []string{"TURSORAFT_ADVERTISE_HOST", "RAFT_ADVERTISE_HOST", "FLY_PRIVATE_IP", "POD_IP", "HOST_IP"} {
		t.Setenv(key, "")
	}
	_ = os.Setenv("HOSTNAME", os.Getenv("HOSTNAME"))

	host, err := resolveAdvertiseHost()
	if err != nil {
		t.Fatalf("resolveAdvertiseHost returned error: %v", err)
	}
	if host == "" {
		t.Fatal("expected non-empty advertise host")
	}
}
