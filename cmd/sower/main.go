package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cristalhq/aconfig"
	"github.com/cristalhq/aconfig/aconfighcl"
	"github.com/cristalhq/aconfig/aconfigtoml"
	"github.com/cristalhq/aconfig/aconfigyaml"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/sower-proxy/deferlog/log"
	"github.com/wweir/sower/router"
)

var (
	version, date string

	conf = struct {
		Remote struct {
			Type     string `default:"sower" required:"true" usage:"option: sower/trojan/socks5/sshd"`
			Addr     string `required:"true" usage:"proxy address, eg: proxy.com/127.0.0.1:7890"`
			User     string `usage:"remote proxy user"`
			Password string `usage:"remote proxy password"`
		}

		DNS struct {
			Disable  bool   `default:"false" usage:"disable DNS proxy"`
			Serve    string `default:"127.0.0.1" required:"true" usage:"dns server ip"`
			Fallback string `default:"223.5.5.5" usage:"fallback dns server"`
		}
		Socks5 struct {
			Disable bool   `default:"false" usage:"disable sock5 proxy"`
			Addr    string `default:":1080" usage:"socks5 listen address"`
		} `flag:"socks5"`

		Router struct {
			Block struct {
				File       string   `usage:"block list file, local file or remote"`
				FilePrefix string   `default:"**." usage:"parsed as '<prefix>line_text'"`
				Rules      []string `usage:"block list rules"`
			}
			Direct struct {
				File       string   `usage:"direct list file, local file or remote"`
				FilePrefix string   `default:"**." usage:"parsed as '<prefix>line_text'"`
				Rules      []string `usage:"direct list rules"`
			}
			Proxy struct {
				File       string   `usage:"proxy list file, local file or remote"`
				FilePrefix string   `default:"**." usage:"parsed as '<prefix>line_text'"`
				Rules      []string `usage:"proxy list rules"`
			}

			Country struct {
				MMDB       string   `usage:"mmdb file"`
				File       string   `usage:"CIDR block list file, local file or remote"`
				FilePrefix string   `default:"" usage:"parsed as '<prefix>line_text'"`
				Rules      []string `usage:"CIDR list rules"`
			}
		}
	}{}
)

func init() {
	if err := aconfig.LoaderFor(&conf, aconfig.Config{
		AllowUnknownFields: true,
		FileFlag:           "f",
		FileDecoders: map[string]aconfig.FileDecoder{
			".yml":  aconfigyaml.New(),
			".yaml": aconfigyaml.New(),
			".toml": aconfigtoml.New(),
			".hcl":  aconfighcl.New(),
		},
	}).Load(); err != nil {
		log.Fatal().Err(err).
			Interface("config", conf).
			Msg("Load config")
	}

	conf.Router.Direct.Rules = append(conf.Router.Direct.Rules,
		conf.Remote.Addr, "**.in-addr.arpa", "**.ip6.arpa")
	log.Info().
		Str("version", version).
		Str("date", date).
		Interface("config", conf).
		Msg("Starting")
}

func main() {
	proxtDial := GenProxyDial(conf.Remote.Type, conf.Remote.Addr, conf.Remote.Password)
	r := router.NewRouter(conf.DNS.Serve, conf.DNS.Fallback, conf.Router.Country.MMDB, proxtDial)
	r.SetBlockRules(conf.Router.Block.Rules)
	r.SetDirectRules(conf.Router.Direct.Rules)
	r.SetProxyRules(conf.Router.Proxy.Rules)
	r.SetCountryCIDRs(conf.Router.Country.Rules)

	go func() {
		if conf.DNS.Disable {
			log.Info().Msg("DNS proxy disabled")
			return
		}

		lnHTTP, err := net.Listen("tcp", net.JoinHostPort(conf.DNS.Serve, "80"))
		if err != nil {
			log.Fatal().Err(err).Msg("listen port")
		}
		go ServeHTTP(lnHTTP, r)

		lnHTTPS, err := net.Listen("tcp", net.JoinHostPort(conf.DNS.Serve, "443"))
		if err != nil {
			log.Fatal().Err(err).Msg("listen port")
		}
		go ServeHTTPS(lnHTTPS, r)

		log.Info().
			Str("listen_on", conf.DNS.Serve).
			Msg("DNS proxy started")
		if err := dns.ListenAndServe(net.JoinHostPort(conf.DNS.Serve, "53"), "udp", r); err != nil {
			log.Fatal().Err(err).Msg("serve dns")
		}
	}()

	go func() {
		if conf.Socks5.Disable {
			log.Info().Msg("SOCKS5 proxy disabled")
			return
		}

		ln, err := net.Listen("tcp", conf.Socks5.Addr)
		if err != nil {
			log.Fatal().Err(err).Msg("listen port")
		}
		log.Info().Msgf("SOCKS5 proxy listening on %s", conf.Socks5.Addr)
		go ServeSocks5(ln, r)
	}()

	start := time.Now()
	r.SetBlockRules(append(conf.Router.Block.Rules,
		loadRules(proxtDial, conf.Router.Block.File, conf.Router.Block.FilePrefix)...))
	r.SetDirectRules(append(conf.Router.Direct.Rules,
		loadRules(proxtDial, conf.Router.Direct.File, conf.Router.Direct.FilePrefix)...))
	r.SetProxyRules(append(conf.Router.Proxy.Rules,
		loadRules(proxtDial, conf.Router.Proxy.File, conf.Router.Proxy.FilePrefix)...))
	r.SetCountryCIDRs(append(conf.Router.Country.Rules,
		loadRules(proxtDial, conf.Router.Country.File, conf.Router.Country.FilePrefix)...))

	log.Info().
		Dur("spend", time.Since(start)).
		Int("blockRule", len(conf.Router.Block.Rules)).
		Int("directRule", len(conf.Router.Direct.Rules)).
		Int("proxyRule", len(conf.Router.Proxy.Rules)).
		Int("countryRule", len(conf.Router.Country.Rules)).
		Msg("Loaded rules, proxy started")
	log.Info().Msg("-X- : blockRule matched")
	log.Info().Msg("--- : directRule matched")
	log.Info().Msg(">>> : proxyRule matched")
	log.Info().Msg("... : no rule matched")
	runtime.GC()
	select {}
}

func loadRules(proxyDial router.ProxyDialFn, file, linePrefix string) []string {
	var loadFn func() (io.ReadCloser, error)
	if _, err := url.Parse(file); err == nil {
		// load rule file from remote by HTTP
		client := &http.Client{
			Transport: &http.Transport{
				Dial: func(network, addr string) (net.Conn, error) {
					domain, port, _ := net.SplitHostPort(addr)
					p, _ := strconv.Atoi(port)
					return proxyDial("tcp", domain, uint16(p))
				},
			},
		}

		loadFn = func() (io.ReadCloser, error) {
			resp, err := client.Get(file)
			if err != nil {
				return nil, err
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return nil, errors.Errorf("status code: %d", resp.StatusCode)
			}

			return resp.Body, nil
		}

	} else {
		// load rule file from local file
		loadFn = func() (io.ReadCloser, error) {
			return os.Open(file)
		}
	}

	// load rule file, retry 10 times
	rc, err := loadFn()
	for i := time.Duration(1); i < 10; i++ {
		if err == nil {
			break
		}

		// wait: 28.5s
		time.Sleep(i * i * 100 * time.Millisecond)
		rc, err = loadFn()
	}
	if err != nil {
		log.Fatal().Err(err).
			Str("file", file).
			Msg("load config file")
	}
	defer rc.Close()

	// parse rule file into rule tree
	var lines []string
	br := bufio.NewReader(rc)
	for {
		line, _, err := br.ReadLine()
		if err == io.EOF {
			break
		} else if err != nil {
			log.Error().Err(err).
				Str("file", file).
				Msg("read line")
			return nil
		}

		if strings.TrimSpace(string(line)) == "" {
			continue
		}

		// use line content as suffix
		lines = append(lines, linePrefix+string(line))
	}

	return lines
}
