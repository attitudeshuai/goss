package system

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goss-org/goss/util"
	"github.com/miekg/dns"
)

type DNS interface {
	Host() string
	Addrs() ([]string, error)
	Resolvable() (bool, error)
	Exists() (bool, error)
	Server() string
	Qtype() string
}

type DefDNS struct {
	ctx        context.Context
	host       string
	resolvable bool
	addrs      []string
	Timeout    int
	loaded     bool
	err        error
	server     string
	qtype      string
}

func NewDefDNS(ctx context.Context, host string, system *System, config util.Config) DNS {
	var h string
	var t string

	splitHost := strings.SplitN(host, ":", 2)
	if len(splitHost) == 2 && regexp.MustCompile(`^[A-Z]+$`).MatchString(splitHost[0]) {
		h = splitHost[1]
		t = splitHost[0]
	} else {
		h = host
	}

	return &DefDNS{
		ctx:     ctx,
		host:    h,
		Timeout: config.TimeOutMilliSeconds(),
		server:  config.Server,
		qtype:   t,
	}
}

func (d *DefDNS) Host() string {
	return d.host
}

func (d *DefDNS) Server() string {
	return d.server
}

func (d *DefDNS) Qtype() string {
	return d.qtype
}

func (d *DefDNS) setup() error {
	if d.loaded {
		return d.err
	}
	d.loaded = true

	for i := 0; i < 3; i++ {
		addrs, err := DNSlookup(d.ctx, d.host, d.server, d.qtype, d.Timeout)
		if err != nil || len(addrs) == 0 {
			d.resolvable = false
			d.addrs = []string{}
			// Do not retry after cancellation; surface it so the check
			// returns promptly.
			if ctxErr := d.ctx.Err(); ctxErr != nil {
				d.err = ctxErr
				return d.err
			}
			// DNSError is resolvable == false, ignore error
			if _, ok := err.(*net.DNSError); ok {
				return nil
			}
			d.err = err
			continue
		}
		sort.Strings(addrs)
		d.resolvable = true
		d.addrs = addrs
		d.err = nil
		return nil
	}
	return d.err
}

func (d *DefDNS) Addrs() ([]string, error) {
	err := d.setup()

	return d.addrs, err
}

func (d *DefDNS) Resolvable() (bool, error) {
	err := d.setup()

	return d.resolvable, err
}

// Stub out
func (d *DefDNS) Exists() (bool, error) {
	return false, nil
}

func DNSlookup(ctx context.Context, host string, server string, qtype string, timeout int) ([]string, error) {
	c1 := make(chan []string, 1)
	e1 := make(chan error, 1)
	timeoutD := time.Duration(timeout) * time.Millisecond

	var addrs []string
	var err error
	go func() {
		if server != "" {
			c := new(dns.Client)
			c.Timeout = timeoutD
			m := new(dns.Msg)

			switch qtype {
			case "A":
				addrs, err = LookupA(ctx, host, server, c, m)
			case "AAAA":
				addrs, err = LookupAAAA(ctx, host, server, c, m)
			case "PTR":
				addrs, err = LookupPTR(ctx, host, server, c, m)
			case "CNAME":
				addrs, err = LookupCNAME(ctx, host, server, c, m)
			case "MX":
				addrs, err = LookupMX(ctx, host, server, c, m)
			case "NS":
				addrs, err = LookupNS(ctx, host, server, c, m)
			case "SRV":
				addrs, err = LookupSRV(ctx, host, server, c, m)
			case "TXT":
				addrs, err = LookupTXT(ctx, host, server, c, m)
			case "SSHFP":
				addrs, err = LookupSSHFP(ctx, host, server, c, m)
			case "CAA":
				addrs, err = LookupCAA(ctx, host, server, c, m)
			default:
				addrs, err = LookupHost(ctx, host, server, c, m)
			}
		} else {
			addrs, err = net.DefaultResolver.LookupHost(ctx, host)
		}
		if err != nil {
			e1 <- err
		}
		c1 <- addrs
	}()
	// A zero timeout means no timeout.
	var timeoutC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeoutD)
		defer timer.Stop()
		timeoutC = timer.C
	}
	select {
	case res := <-c1:
		return res, nil
	case err := <-e1:
		return nil, err
	case <-timeoutC:
		return nil, fmt.Errorf("DNS lookup timed out (%s)", timeoutD)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A and AAAA record lookup - similar to net.LookupHost
func LookupHost(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	a, _ := LookupA(ctx, host, server, c, m)
	aaaa, _ := LookupAAAA(ctx, host, server, c, m)
	addrs = append(a, aaaa...)

	return
}

// A record lookup
func LookupA(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeA)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.A); ok {
			addrs = append(addrs, t.A.String())
		}
	}

	return
}

// parseServerString - Check if the DNS Server in server config has a port, if not ensure 53 is prefixed.
func parseServerString(server string) string {
	srvhost, srvport, err := net.SplitHostPort(server)
	if err != nil {
		srvport = "53"
		srvhost = server
	}
	return net.JoinHostPort(srvhost, srvport)
}

// AAAA (IPv6) record lookup
func LookupAAAA(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeAAAA)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.AAAA); ok {
			addrs = append(addrs, t.AAAA.String())
		}
	}

	return
}

// CNAME record lookup
func LookupCNAME(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeCNAME)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.CNAME); ok {
			addrs = append(addrs, t.Target)
		}
	}

	return
}

// MX record lookup
func LookupMX(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeMX)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.MX); ok {
			mxstring := strconv.Itoa(int(t.Preference)) + " " + t.Mx
			addrs = append(addrs, mxstring)
		}
	}

	return
}

// NS record lookup
func LookupNS(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeNS)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.NS); ok {
			addrs = append(addrs, t.Ns)
		}
	}

	return
}

// SRV record lookup
func LookupSRV(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeSRV)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.SRV); ok {
			prio := strconv.Itoa(int(t.Priority))
			weight := strconv.Itoa(int(t.Weight))
			port := strconv.Itoa(int(t.Port))
			srvrec := strings.Join([]string{prio, weight, port, t.Target}, " ")
			addrs = append(addrs, srvrec)
		}
	}

	return
}

// SSHFP record lookup
func LookupSSHFP(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeSSHFP)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.SSHFP); ok {
			algo := strconv.Itoa(int(t.Algorithm))
			fpType := strconv.Itoa(int(t.Type))
			fingerprint := strings.ToUpper(t.FingerPrint)
			sshfpRec := strings.Join([]string{algo, fpType, fingerprint}, " ")
			addrs = append(addrs, sshfpRec)
		}
	}

	return
}

// TXT record lookup
func LookupTXT(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeTXT)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.TXT); ok {
			addrs = append(addrs, t.Txt...)
		}
	}

	return
}

// PTR record lookup
func LookupPTR(ctx context.Context, addr string, server string, c *dns.Client, m *dns.Msg) (name []string, err error) {

	reverse, err := dns.ReverseAddr(addr)
	if err != nil {
		return nil, err
	}

	m.SetQuestion(reverse, dns.TypePTR)

	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		name = append(name, ans.(*dns.PTR).Ptr)
	}

	return
}

// CAA record lookup
func LookupCAA(ctx context.Context, host string, server string, c *dns.Client, m *dns.Msg) (addrs []string, err error) {
	m.SetQuestion(dns.Fqdn(host), dns.TypeCAA)
	r, _, err := c.ExchangeContext(ctx, m, parseServerString(server))
	if err != nil {
		return nil, err
	}

	for _, ans := range r.Answer {
		if t, ok := ans.(*dns.CAA); ok {
			flag := strconv.Itoa(int(t.Flag))
			caarec := strings.Join([]string{flag, t.Tag, t.Value}, " ")
			addrs = append(addrs, caarec)
		}
	}

	return
}
