package config

import (
	"bytes"
	"io"
	"net"
)

func newReader(data []byte) io.Reader { return bytes.NewReader(data) }

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}
