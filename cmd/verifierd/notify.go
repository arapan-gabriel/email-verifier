package main

import (
	"net"
	"os"
)

// sd_notify, hand-rolled.
//
// Type=simple tells systemd the unit is up the moment ExecStart forks, which is
// before the listener is bound: `systemctl restart verifierd` returns and the
// next connection is refused. That is invisible interactively and fatal to a
// deploy script, which cannot then tell a healthy restart from a crash loop.
// Type=notify moves that decision to us — we say READY=1 once the socket is
// actually accepting.
//
// The protocol is one datagram of newline-separated key=value pairs to the
// unix socket named by NOTIFY_SOCKET, so it costs a dozen lines rather than a
// dependency. A leading '@' means the abstract namespace, whose name starts
// with a NUL byte.
func sdNotify(state string) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return // not under systemd, or NotifyAccess denies us: nothing to tell
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		// Readiness reporting is not worth failing a start over; systemd's
		// TimeoutStartSec is the backstop if we never report.
		return
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte(state))
}
