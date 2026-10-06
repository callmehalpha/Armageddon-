//go:build !linux

package server

import (
	"errors"
	"net"
)

func socketPeer(net.Conn) (int, uint32, error) { return 0, 0, errors.New("unsupported") }

func parentPID(int) int { return 0 }

// Without SO_PEERCRED the IDE proxy cannot check who serves the socket;
// workspace isolation is a Linux-only feature anyway.
const peerCheckSupported = false
