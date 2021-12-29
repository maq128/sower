package main

import (
	"bufio"
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/sower-proxy/conns/relay"
	"github.com/sower-proxy/conns/teeconn"
	"github.com/sower-proxy/deferlog/log"
	"github.com/wweir/sower/router"
	"github.com/wweir/sower/transport"
	"github.com/wweir/sower/transport/socks5"
	"github.com/wweir/sower/transport/sower"
	"github.com/wweir/sower/transport/ssh"
	"github.com/wweir/sower/transport/trojan"

	crypto_ssh "golang.org/x/crypto/ssh"
)

func GenProxyDial(proxyType, proxyHost, proxyPassword string) router.ProxyDialFn {
	var proxy transport.Transport
	var dialFn func(host string, port uint16) (net.Conn, error)

	switch conf.Remote.Type {
	case "sower":
		proxy = sower.New(conf.Remote.Password)
		tlsCfg := &tls.Config{}
		dialFn = func(host string, port uint16) (net.Conn, error) {
			return tls.Dial("tcp", net.JoinHostPort(proxyHost, "443"), tlsCfg)
		}

	case "trojan":
		proxy = trojan.New(conf.Remote.Password)
		tlsCfg := &tls.Config{}
		dialFn = func(host string, port uint16) (net.Conn, error) {
			return tls.Dial("tcp", net.JoinHostPort(proxyHost, "443"), tlsCfg)
		}

	case "socks5":
		proxy = socks5.New()
		dialFn = func(host string, port uint16) (net.Conn, error) {
			return net.Dial("tcp", proxyHost)
		}

	case "sshd":
		config := crypto_ssh.ClientConfig{
			User:            conf.Remote.User,
			Auth:            []crypto_ssh.AuthMethod{crypto_ssh.Password(conf.Remote.Password)},
			HostKeyCallback: crypto_ssh.InsecureIgnoreHostKey(),
		}
		sshClient, err := crypto_ssh.Dial("tcp", proxyHost, &config)
		if err != nil {
			log.Fatal().Msg("connect to sshd failed")
		}

		proxy = ssh.New()
		dialFn = func(host string, port uint16) (net.Conn, error) {
			return sshClient.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
		}

	default:
		log.Fatal().
			Str("type", conf.Remote.Type).
			Msg("unknown proxy type")
	}

	return func(network, host string, port uint16) (net.Conn, error) {
		if host == "" || port == 0 {
			return nil, errors.Errorf("invalid addr(%s:%d)", host, port)
		}

		conn, err := dialFn(host, port)
		if err != nil {
			return nil, err
		}

		if err := proxy.Wrap(conn, host, port); err != nil {
			conn.Close()
			return nil, err
		}

		return conn, nil
	}
}

func ServeHTTP(ln net.Listener, r *router.Router) {
	conn, err := ln.Accept()
	if err != nil {
		log.Fatal().Err(err).
			Msg("serve socks5")
	}

	go ServeHTTP(ln, r)
	start := time.Now()
	teeconn := teeconn.New(conn)
	defer teeconn.Close()

	req, err := http.ReadRequest(bufio.NewReader(teeconn))
	if err != nil {
		log.Error().Err(err).Msg("read http request")
		return
	}

	rc, err := r.ProxyDial("tcp", req.Host, 80)
	if err != nil {
		log.Error().Err(err).
			Str("host", req.Host).
			Interface("req", req.URL).
			Msg("dial proxy")
		return
	}
	defer rc.Close()

	teeconn.Stop().Reread()
	relay.Relay(teeconn, rc)
	log.Debug().
		Str("host", req.Host).
		Dur("spend", time.Since(start)).
		Msg("serve http")
}

func ServeHTTPS(ln net.Listener, r *router.Router) {
	conn, err := ln.Accept()
	if err != nil {
		log.Fatal().Err(err).
			Msg("serve socks5")
	}

	go ServeHTTPS(ln, r)
	start := time.Now()
	teeconn := teeconn.New(conn)
	defer teeconn.Close()

	var domain string
	tls.Server(teeconn, &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			domain = hello.ServerName
			return nil, nil
		},
	}).Handshake()

	rc, err := r.ProxyDial("tcp", domain, 443)
	if err != nil {
		log.Error().Err(err).
			Str("host", domain).
			Msg("dial proxy")
		return
	}
	defer rc.Close()

	teeconn.Stop().Reread()
	relay.Relay(teeconn, rc)
	log.Debug().
		Str("host", domain).
		Dur("spend", time.Since(start)).
		Msg("serve http")
}

func ServeSocks5(ln net.Listener, r *router.Router) {
	conn, err := ln.Accept()
	if err != nil {
		log.Fatal().Err(err).
			Msg("serve socks5")
	}
	go ServeSocks5(ln, r)
	defer conn.Close()

	addr, err := socks5.New().Unwrap(conn)
	if err != nil {
		log.Warn().Err(err).
			Msgf("parse socks5 target: %s", addr)
		return
	}

	host, port := addr.(*socks5.AddrHead).Addr()
	r.RouteHandle(conn, host, port)
}
