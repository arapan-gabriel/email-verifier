package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// The point of Type=notify is that a deploy can trust `systemctl restart`, so
// the datagram actually reaching systemd is the whole contract.
func TestSDNotifySendsState(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "notify.sock")

	ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer func() { _ = ln.Close() }()

	t.Setenv("NOTIFY_SOCKET", sock)
	sdNotify("READY=1")

	if err := ln.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := ln.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Fatalf("state = %q, want %q", got, "READY=1")
	}
}

// Without NOTIFY_SOCKET the binary must still run — it is started by hand and
// in tests far more often than by systemd.
func TestSDNotifyWithoutSocketIsSilent(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	sdNotify("READY=1") // must not panic or block
}

// A path systemd never created must not take the process down either.
func TestSDNotifyToMissingSocketIsSilent(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	sdNotify("READY=1")
}
