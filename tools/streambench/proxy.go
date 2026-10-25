package main

// The benchmark proxy: the bench client talks to it, it forwards to the real device
// through impaired pipes. For WebRTC it acts like a NAT between the two ICE agents
// (each side's SDP is rewritten to point at the proxy), so RTP, RTCP and data
// channels all cross the impaired path. The WebSocket can be proxied too (-ws-proxy),
// for the WebCodecs fallback path; by default it stays direct, like control traffic
// staying on DERP while media goes direct.

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// udpProxy forwards between the bench (loopback) and the device, impairing both ways.
type udpProxy struct {
	bench *net.UDPConn // unconnected listener on 127.0.0.1; the bench's ICE target
	dev   *net.UDPConn // connected toward the device; its address is advertised to the device

	upImpair, downImpair *pathImpair
	upSched, downSched   *scheduler

	mu   sync.Mutex
	peer *net.UDPAddr // the bench's ICE address, learned from its first packet

	// packet counters for debugging whether ICE actually crossed the proxy
	upPkts, downPkts atomic.Int64
}

// newUDPProxy forwards between the bench and the device, impairing both ways. Both
// sockets live on hostIP: the bench-facing one is the remote candidate the bench
// knows (its packets must come back from exactly that address), the device-facing
// one is the candidate the device knows, likewise.
func newUDPProxy(device *net.UDPAddr, hostIP net.IP, up, down *pathImpair, upS, downS *scheduler) (*udpProxy, error) {
	a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: hostIP})
	if err != nil {
		return nil, err
	}
	d, err := net.DialUDP("udp4", nil, device)
	if err != nil {
		a.Close()
		return nil, err
	}
	p := &udpProxy{
		bench: a, dev: d,
		upImpair: up, downImpair: down, upSched: upS, downSched: downS,
	}
	go p.fromBench()
	go p.fromDevice()
	return p, nil
}

func (p *udpProxy) fromBench() {
	buf := make([]byte, 2048)
	for {
		n, from, err := p.bench.ReadFromUDP(buf)
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.peer == nil {
			p.peer = from
		}
		p.mu.Unlock()
		p.upPkts.Add(1)
		p.forward(p.upImpair, p.upSched, buf[:n], func(b []byte) { p.dev.Write(b) })
	}
}

func (p *udpProxy) fromDevice() {
	buf := make([]byte, 2048)
	for {
		n, err := p.dev.Read(buf)
		if err != nil {
			return
		}
		p.mu.Lock()
		peer := p.peer
		p.mu.Unlock()
		if peer == nil {
			continue
		}
		p.downPkts.Add(1)
		p.forward(p.downImpair, p.downSched, buf[:n], func(b []byte) { p.bench.WriteToUDP(b, peer) })
	}
}

// forward applies the impairment and hands the datagram to the scheduler.
func (p *udpProxy) forward(imp *pathImpair, s *scheduler, data []byte, send func([]byte)) {
	now := time.Now()
	if d, queued := imp.admit(len(data), now); d >= 0 {
		cp := append([]byte(nil), data...)
		s.add(now.Add(d), func() {
			send(cp)
			if queued {
				imp.sent(len(cp))
			}
		})
	}
}

func (p *udpProxy) close() {
	p.bench.Close()
	p.dev.Close()
}

// stats reports how many datagrams crossed each direction.
func (p *udpProxy) stats() (up, down int64) { return p.upPkts.Load(), p.downPkts.Load() }

// legAAddr is the address the bench is told to send to (rewritten into the answer).
func (p *udpProxy) legAAddr() *net.UDPAddr { return p.bench.LocalAddr().(*net.UDPAddr) }

// devAddr is the source address packets toward the device come from; the device's
// answers come back to exactly this address, so it is what we advertise in the offer.
func (p *udpProxy) devAddr() *net.UDPAddr { return p.dev.LocalAddr().(*net.UDPAddr) }

var candRe = regexp.MustCompile(`(?i)a=candidate:\S+ \d+ udp \d+ (\S+) (\d+) typ host`)

// rewriteOffer points the device at our device-facing socket instead of the bench's
// own address (which the device may not be able to route to).
func rewriteOffer(sdp string, muxPort int, dev *net.UDPAddr) (string, bool) {
	re := regexp.MustCompile(fmt.Sprintf(`(?i)(\S+) %d typ host`, muxPort))
	if !re.MatchString(sdp) {
		return sdp, false
	}
	out := re.ReplaceAllString(sdp, fmt.Sprintf("%s %d typ host", dev.IP, dev.Port))
	return out, out != sdp
}

// rewriteAnswer points the bench at our leg-A listener instead of the device's address.
func rewriteAnswer(sdp string, legA *net.UDPAddr) (string, bool) {
	out := candRe.ReplaceAllStringFunc(sdp, func(line string) string {
		m := candRe.FindStringSubmatch(line)
		return strings.Replace(line, m[1]+" "+m[2], fmt.Sprintf("%s %d", legA.IP, legA.Port), 1)
	})
	return out, out != sdp
}

// tcpProxy listens locally and forwards to the real wss endpoint through impairments.
func tcpProxy(listenAddr string, target *url.URL, down, up *pathImpair) (net.Addr, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	host := target.Hostname()
	port := "443"
	if target.Port() != "" {
		port = target.Port()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				d, err := tls.Dial("tcp", net.JoinHostPort(host, port), &tls.Config{ServerName: host})
				if err != nil {
					return
				}
				defer d.Close()
				go func() { // device → bench (the video): the impaired downlink
					io.Copy(throttleConn{Conn: c, p: down}, d)
					c.Close()
				}()
				io.Copy(throttleConn{Conn: d, p: up}, c) // bench → device (small)
				d.Close()
			}(c)
		}
	}()
	return ln.Addr(), nil
}
