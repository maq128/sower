package ssh

import (
	"net"
)

type Ssh struct{}

func New() *Ssh {
	return &Ssh{}
}

func (s *Ssh) Unwrap(conn net.Conn) (net.Addr, error) {
	return nil, nil
}

func (s *Ssh) Wrap(conn net.Conn, tgtHost string, tgtPort uint16) error {
	return nil
}
