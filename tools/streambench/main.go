// streambench measures the stream the way a browser sees it, through an impaired
// network, so runs are comparable.
//
//	go run ./tools/streambench -t 20 https://devices.<tailnet>.ts.net/mac/
//	go run ./tools/streambench -t 30 -loss 2 -jitter 15ms -down 8Mbps -rate-at 10s:3Mbps ...
//
// What it reports: capture-to-delivery latency per frame (exact on the WebSocket
// path, where frames carry capture timestamps; floor-relative on WebRTC, where the
// RTP timestamp base is unknown — the floor itself is measured on the WebSocket
// frames that flow before WebRTC takes over), FPS (average and worst seconds),
// bitrate, packet loss, keyframe recovery after a `kf` request, input round trip
// on the data channel, and recovery from a blackout or a mid-stream bandwidth drop.
//
// The proxy sits between bench and device and applies loss, delay+jitter, a
// token-bucket rate limit with a small queue (tail drop, like a bottleneck buffer),
// scheduled rate changes and blackouts. For WebRTC it acts like a NAT (both sides'
// ICE candidates are rewritten to point at it), so RTP, RTCP and data channels all
// cross it. Frames come from an animated window: build tools/pattern first.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type durValue time.Duration

func (d *durValue) String() string { return time.Duration(*d).String() }
func (d *durValue) Set(s string) error {
	v, err := time.ParseDuration(s)
	*d = durValue(v)
	return err
}

type bpsValue int64

func (b bpsValue) String() string { return fmt.Sprintf("%d", int64(b)) }
func (b *bpsValue) Set(s string) error {
	m := map[string]float64{"kbps": 1e3, "mbps": 1e6, "gbps": 1e9}
	s = strings.ToLower(s)
	for suf, mul := range m {
		if strings.HasSuffix(s, suf) {
			v, err := fmtSscanf(s[:len(s)-len(suf)])
			*b = bpsValue(v * mul)
			return err
		}
	}
	v, err := fmtSscanf(s)
	*b = bpsValue(v)
	return err
}

func fmtSscanf(s string) (float64, error) {
	var v float64
	_, err := fmt.Sscanf(s, "%g", &v)
	return v, err
}

// schedFlag parses "5s:1.5Mbps,12s:300kbps": a time and a value per entry.
type schedEntry struct {
	at  time.Duration
	bps int64
}

type schedFlag []schedEntry

func (s schedFlag) String() string { return "" }
func (s *schedFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		f := strings.SplitN(part, ":", 2)
		if len(f) != 2 {
			return fmt.Errorf("want time:value, got %q", part)
		}
		at, err := time.ParseDuration(f[0])
		if err != nil {
			return err
		}
		var b bpsValue
		if err := b.Set(f[1]); err != nil {
			return err
		}
		*s = append(*s, schedEntry{at, int64(b)})
	}
	return nil
}

func main() {
	var (
		video   = flag.String("video", "rtc", "rtc | ws")
		secs    = flag.Int("t", 20, "seconds to run")
		maxbr   = bpsValue(20_000_000)
		w       = flag.Int("w", 2560, "max stream width/height in px")
		fps     = flag.Int("fps", 60, "requested fps")
		display = flag.Int("display", 0, "display ID (0 = main)")

		loss   = flag.Float64("loss", 0, "random datagram loss % (downlink)")
		delay  = durValue(0)
		jitter = durValue(0)
		down   = bpsValue(0)
		up     = bpsValue(0)
		queue  = flag.Int("queue-bytes", 128<<10, "bottleneck queue before tail drop")

		wsProxy  = flag.Bool("ws-proxy", false, "send the WebSocket through the impaired proxy too")
		blackout = flag.String("blackout", "", "e.g. 12s:800ms — kill the link at t for that long")
		rateAt   schedFlag
		kfAt     = flag.String("kf-at", "", "e.g. 6s,14s — request a keyframe at these times")
		jsonOut  = flag.Bool("json", false, "print only the JSON report")
		rtcFirst = flag.Bool("rtc-first", false, "like the viewer: no WebSocket video while WebRTC connects")
		pionLog  = flag.String("pionlog", "", "comma-separated pion log scopes at debug level (e.g. ice,dtls)")
	)
	flag.Var(&maxbr, "maxbr", "max bitrate (e.g. 20Mbps)")
	flag.Var(&delay, "delay", "one-way delay (e.g. 20ms)")
	flag.Var(&jitter, "jitter", "0..jitter extra delay, uniform (e.g. 10ms)")
	flag.Var(&down, "down", "downlink bandwidth cap (e.g. 6Mbps)")
	flag.Var(&up, "up", "uplink bandwidth cap")
	flag.Var(&rateAt, "rate-at", "scheduled downlink rate changes, e.g. 10s:3Mbps")
	flag.Parse()

	page, err := url.Parse(flag.Arg(0))
	if err != nil || page.Host == "" {
		fmt.Fprintln(os.Stderr, "usage: streambench [flags] https://<device>/")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*secs)*time.Second+5*time.Second)
	defer cancel()

	run := &bench{
		video: *video, maxbr: int64(maxbr), w: *w, fps: *fps, display: *display,
		jsonOut: *jsonOut, holdWS: *rtcFirst, loss: *loss, delay: time.Duration(delay), jitter: time.Duration(jitter),
		down: int64(down), up: int64(up), queue: *queue, page: page,
	}
	run.rateAt = rateAt
	if *blackout != "" {
		f := strings.SplitN(*blackout, ":", 2)
		at, err1 := time.ParseDuration(f[0])
		d, err2 := time.ParseDuration(f[1])
		if err1 != nil || err2 != nil {
			fmt.Fprintln(os.Stderr, "bad -blackout")
			os.Exit(2)
		}
		run.blackoutAt, run.blackoutDur = at, d
	}
	for _, s := range strings.Split(*kfAt, ",") {
		if s = strings.TrimSpace(s); s != "" {
			if at, err := time.ParseDuration(s); err == nil {
				run.kfAt = append(run.kfAt, at)
			}
		}
	}
	run.wsProxy = *wsProxy || *video == "ws"
	run.pionLog = *pionLog

	if err := run.start(ctx); err != nil {
		fmt.Println("bench:", err)
		os.Exit(1)
	}
	<-ctx.Done()
	rep := run.finish()
	if !*jsonOut {
		up, down := run.udp.stats()
		fmt.Printf("proxy: %d datagrams bench→device, %d device→bench\n", up, down)
	}
	fmt.Println(rep.String())
}

// ---- the bench itself ----

type bench struct {
	video                   string
	maxbr                   int64
	w, fps, display, queue  int
	loss                    float64
	delay, jitter           time.Duration
	down, up                int64
	rateAt                  []schedEntry
	blackoutAt, blackoutDur time.Duration
	kfAt                    []time.Duration
	wsProxy, jsonOut        bool
	holdWS                  bool // -rtc-first
	page                    *url.URL

	startAt time.Time
	ws      *websocket.Conn
	wsMu    sync.Mutex

	rep  report
	mu   sync.Mutex
	latWS  []float64 // absolute capture→deliver ms (ws frames)
	latRTC []float64 // floor-relative ms
	gaps   []float64 // inter-frame arrival deltas, ms
	seq    seqTracker
	firstRTPArr float64 // unix ms of the first RTP packet
	firstRTPTs  uint32  // its timestamp
	rtcFirst, wsFirst time.Time
	lastFrameAt  time.Time
	kfPending    []time.Time
	kfRecover    []float64
	rtts         []float64
	frameArrival []time.Time
	secLats      map[int][]float64
	firstFrameAt time.Time
	tsDump       int

	pc      *webrtc.PeerConnection
	inputDC *webrtc.DataChannel
	iceIP   net.IP

	upImp, downImp *pathImpair
	udp            *udpProxy
	pionLog        string
}

func (b *bench) send(v any) {
	j, _ := json.Marshal(v)
	b.wsMu.Lock()
	b.ws.Write(context.Background(), websocket.MessageText, j)
	b.wsMu.Unlock()
}

func (b *bench) sendInput(v any) {
	if b.inputDC != nil && b.inputDC.ReadyState() == webrtc.DataChannelStateOpen {
		j, _ := json.Marshal(v)
		b.inputDC.SendText(string(j))
	} else {
		b.send(v)
	}
}

func (b *bench) start(ctx context.Context) error {
	b.rep.Mode = b.video

	// Impairment: UDP both ways for WebRTC, TCP too if the WebSocket is proxied.
	upS, downS := newScheduler(), newScheduler()
	var black []blackout
	if b.blackoutDur > 0 {
		black = append(black, blackout{start: time.Now().Add(b.blackoutAt), dur: b.blackoutDur})
	}
	b.upImp = newPath(0, b.delay, b.jitter, b.up, b.queue, nil, upS)
	b.downImp = newPath(b.loss, b.delay, b.jitter, b.down, b.queue, black, downS)
	b.downImp.mu.Lock()
	for _, e := range b.rateAt {
		b.downImp.changes = append(b.downImp.changes, rateChange{at: time.Now().Add(e.at), bps: e.bps})
	}
	b.downImp.mu.Unlock()

	// WebRTC media goes to the device's ICE port on its tailnet address.
	devIP, err := net.ResolveIPAddr("ip4", b.page.Hostname())
	if err != nil {
		return err
	}
	devUDP := &net.UDPAddr{IP: devIP.IP, Port: 50050}
	// The bench's own ICE socket (and the proxy's bench-facing leg) must be on an
	// address the device can route to: the host's Tailscale IP (the device's netstack
	// reaches the host through the tailnet). Loopback is skipped by candidate
	// gathering and the LAN IP isn't routed by the device's netstack.
	hostIP := net.ParseIP(tailscaleIP())
	if hostIP == nil {
		hostIP = net.IPv4(127, 0, 0, 1)
	}
	b.iceIP = hostIP
	if b.udp, err = newUDPProxy(devUDP, hostIP, b.upImp, b.downImp, upS, downS); err != nil {
		return err
	}

	wsURL := url.URL{Scheme: "wss", Host: b.page.Host, Path: strings.TrimSuffix(b.page.Path, "/") + "/stream"}
	if b.wsProxy {
		addr, err := tcpProxy("127.0.0.1:0", &url.URL{Scheme: "https", Host: b.page.Host}, b.downImp, b.upImp)
		if err != nil {
			return err
		}
		wsURL.Scheme, wsURL.Host = "ws", addr.String()
	}
	ws, _, err := websocket.Dial(ctx, wsURL.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin": {"https://" + b.page.Host},
			"Cookie": {testCookie()},
		},
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL.String(), err)
	}
	ws.SetReadLimit(64 << 20)
	b.ws = ws
	b.startAt = time.Now()

	b.send(map[string]any{"t": "start", "w": b.w, "h": b.w, "fps": b.fps, "maxbr": b.maxbr,
		"localCursor": true, "display": b.display, "rtcFirst": b.holdWS && b.video == "rtc"})

	if b.video == "rtc" {
		if err := b.startRTC(); err != nil {
			return err
		}
	}
	go b.readWS(ctx)

	go func() { // input round trips
		for ctx.Err() == nil {
			b.sendInput(map[string]any{"t": "ping", "at": time.Now().UnixMicro()})
			time.Sleep(250 * time.Millisecond)
		}
	}()
	for _, at := range b.kfAt {
		time.AfterFunc(at-(time.Since(b.startAt)), func() {
			b.mu.Lock()
			b.kfPending = append(b.kfPending, time.Now())
			b.mu.Unlock()
			b.send(map[string]any{"t": "kf"})
		})
	}
	// Per-second status line: print second i's record one tick late so its data is complete.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var lastDrops int64
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			var drops int64
			if b.downImp != nil {
				b.downImp.mu.Lock()
				drops = b.downImp.drops
				b.downImp.mu.Unlock()
			}
			b.mu.Lock()
			b.rep.second(i).ProxyDrops = drops - lastDrops
			lastDrops = drops
			s := b.rep.seriesAt(i - 1)
			if !b.jsonOut {
				fmt.Printf("t=%2ds fps=%3d %5.1f Mbps loss=%4.1f%% lat95=%6s br=%2dM drops=%d rtt=%sms\n",
					i-1, s.Frames, float64(s.Bytes)*8/1e6, b.seq.lossPct(),
					fmtMS(pct(b.secLats[i-1], 0.95)), s.Br/1_000_000, s.Drops, fmtMS(s.InputRTT))
			}
			b.mu.Unlock()
		}
	}()
	return nil
}

// startRTC builds the bench's peer connection the way a browser would: TWCC feedback
// and NACK generation on the receive side, one UDP socket, all media via the proxy.
func (b *bench) startRTC() error {
	m := &webrtc.MediaEngine{}
	feedback := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "transport-cc"}}
	for pt, profile := range map[webrtc.PayloadType]string{102: "640c1f", 106: "42e01f"} {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: 90000, RTCPFeedback: feedback,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile,
			},
			PayloadType: pt,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return err
	}

	iceConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: b.iceIP})
	if err != nil {
		return err
	}
	se := webrtc.SettingEngine{}
	if b.pionLog != "" {
		lf := logging.NewDefaultLoggerFactory()
		lf.DefaultLogLevel = logging.LogLevelTrace
		se.LoggerFactory = lf
	}
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, iceConn))
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled) // the device must see a routable address

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	b.pc = pc
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return err
	}

	in, err := pc.CreateDataChannel("input", nil)
	if err != nil {
		return err
	}
	mv, err := pc.CreateDataChannel("move", &webrtc.DataChannelInit{Ordered: new(bool), MaxRetransmits: new(uint16)})
	if err != nil {
		return err
	}
	sping := func(dc *webrtc.DataChannel) func(webrtc.DataChannelMessage) {
		return func(m webrtc.DataChannelMessage) {
			var ev map[string]any
			if json.Unmarshal(m.Data, &ev) != nil || ev["t"] != "sping" {
				return
			}
			ev["t"] = "spong"
			if b, err := json.Marshal(ev); err == nil {
				dc.SendText(string(b))
			}
		}
	}
	mv.OnMessage(sping(mv))
	in.OnOpen(func() { b.inputDC = in })
	in.OnMessage(func(m webrtc.DataChannelMessage) {
		var ev map[string]any
		if json.Unmarshal(m.Data, &ev) != nil {
			return
		}
		if ev["t"] == "sping" { // the server's uplink probe (see rtc.go pingLoop)
			ev["t"] = "spong"
			if b, err := json.Marshal(ev); err == nil {
				in.SendText(string(b))
			}
			return
		}
		if ev["t"] == "pong" {
			rtt := float64(time.Now().UnixMicro()-int64(ev["at"].(float64))) / 1000
			b.mu.Lock()
			b.rtts = append(b.rtts, rtt)
			b.rep.second(int(time.Since(b.startAt).Seconds())).InputRTT = rtt
			b.mu.Unlock()
		}
	})

	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if !b.jsonOut {
			fmt.Printf("track: %s %s\n", t.Codec().MimeType, t.Codec().SDPFmtpLine)
		}
		buf := make([]byte, 2048)
		for {
			n, _, err := t.Read(buf)
			if err != nil {
				return
			}
			b.onRTP(buf[:n])
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if !b.jsonOut {
			fmt.Printf("webrtc %s after %v\n", s, time.Since(b.startAt).Round(time.Millisecond))
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-time.After(3 * time.Second):
	}
	sdp := pc.LocalDescription().SDP
	muxPort := iceConn.LocalAddr().(*net.UDPAddr).Port
	sdp, ok := rewriteOffer(sdp, muxPort, b.udp.devAddr())
	if !ok && !b.jsonOut {
		for _, line := range strings.Split(sdp, "\r\n") {
			if strings.Contains(line, "candidate") {
				fmt.Println("offer candidate:", line)
			}
		}
		fmt.Printf("note: no %s:%d candidate in the offer; ICE may bypass the proxy\n", b.iceIP, muxPort)
	}
	b.send(map[string]any{"t": "rtc-offer", "sdp": sdp})
	return nil
}

// onRTP records one incoming RTP packet.
func (b *bench) onRTP(p []byte) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(p); err != nil {
		return
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rtcFirst.IsZero() {
		b.rtcFirst = now
		b.firstRTPArr = float64(now.UnixNano()) / 1e6
		b.firstRTPTs = pkt.Timestamp
	}
	b.seq.push(uint16(pkt.SequenceNumber))
	s := b.rep.second(int(now.Sub(b.startAt).Seconds()))
	s.Bytes += len(pkt.Payload)

	// Floor-relative latency: the RTP base is unknown, so everything is measured
	// from the first packet and the run's minimum is subtracted at the end.
	tsMs := float64(int32(pkt.Timestamp-b.firstRTPTs)) * 1000 / 90000
	lat := float64(now.UnixNano())/1e6 - b.firstRTPArr - tsMs
	if b.tsDump < 120 && os.Getenv("MD_TSDEBUG") != "" {
		b.tsDump++
		fmt.Printf("pkt seq=%d ts=%d tsMs=%.1f arrMs=%.1f lat=%.1f\n",
			pkt.SequenceNumber, pkt.Timestamp, tsMs, float64(now.UnixNano())/1e6-b.firstRTPArr, lat)
	}
	b.latRTC = append(b.latRTC, lat)
	sec := int(now.Sub(b.startAt).Seconds())
	if b.secLats == nil {
		b.secLats = map[int][]float64{}
	}
	b.secLats[sec] = append(b.secLats[sec], lat)
	if b.firstFrameAt.IsZero() {
		b.firstFrameAt = now
	}

	if pkt.Marker {
		s.Frames++
		if !b.lastFrameAt.IsZero() {
			b.gaps = append(b.gaps, float64(now.Sub(b.lastFrameAt))/float64(time.Millisecond))
		}
		b.lastFrameAt = now
		b.frameArrival = append(b.frameArrival, now)
	}
	if hasIDR(pkt.Payload) {
		b.onKeyframe(now)
	}
}

func (b *bench) onKeyframe(now time.Time) {
	if len(b.kfPending) > 0 {
		req := b.kfPending[0]
		b.kfPending = b.kfPending[1:]
		b.kfRecover = append(b.kfRecover, float64(now.Sub(req))/float64(time.Millisecond))
	}
}

// readWS handles control events and (in ws mode) video frames.
func (b *bench) readWS(ctx context.Context) {
	for {
		typ, data, err := b.ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var ev map[string]any
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			switch ev["t"] {
			case "rtc-answer":
				raw, _ := ev["sdp"].(string)
				sdp, ok := rewriteAnswer(raw, b.udp.legAAddr())
				if !ok && !b.jsonOut {
					for _, line := range strings.Split(raw, "\r\n") {
						if strings.Contains(line, "candidate") {
							fmt.Println("answer candidate:", line)
						}
					}
					fmt.Println("note: answer had no rewritten host candidate; ICE may bypass the proxy")
				}
				b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
			case "stats":
				b.mu.Lock()
				s := b.rep.second(int(time.Since(b.startAt).Seconds()))
				s.Br = int64(ev["br"].(float64))
				if d, ok := ev["drops"].(float64); ok {
					s.Drops = int64(d)
				}
				if v, ok := ev["est"].(float64); ok {
					s.Est = int64(v)
				}
				if v, ok := ev["act"].(float64); ok {
					s.Act = int64(v)
				}
				if v, ok := ev["bl"].(float64); ok {
					s.BL = int(v)
				}
				b.mu.Unlock()
			case "pong":
				rtt := float64(time.Now().UnixMicro()-int64(ev["at"].(float64))) / 1000
				b.mu.Lock()
				b.rtts = append(b.rtts, rtt)
				b.rep.second(int(time.Since(b.startAt).Seconds())).InputRTT = rtt
				b.mu.Unlock()
			}
			continue
		}
		if len(data) > 10 && data[0] == 2 { // [u8 key][u64 pts µs][AVCC]
			b.onWSFrame(binary.BigEndian.Uint64(data[2:10]), data[1] == 1, len(data), time.Now())
		}
	}
}

func (b *bench) onWSFrame(ptsUs uint64, key bool, n int, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.wsFirst.IsZero() {
		b.wsFirst = now
	}
	s := b.rep.second(int(now.Sub(b.startAt).Seconds()))
	s.Frames++
	s.Bytes += n
	if !b.lastFrameAt.IsZero() {
		b.gaps = append(b.gaps, float64(now.Sub(b.lastFrameAt))/float64(time.Millisecond))
	}
	b.lastFrameAt = now
	b.frameArrival = append(b.frameArrival, now)

	// Both the helper's timestamps and our clock are on the mach host clock, so
	// capture→deliver is a plain subtraction.
	lat := hostNowMs() - float64(ptsUs)/1000
	b.latWS = append(b.latWS, lat)
	if b.secLats == nil {
		b.secLats = map[int][]float64{}
	}
	sec := int(now.Sub(b.startAt).Seconds())
	b.secLats[sec] = append(b.secLats[sec], lat)
	if b.firstFrameAt.IsZero() {
		b.firstFrameAt = now
	}
	if key {
		b.onKeyframe(now)
	}
}

// finish computes the final report.
func (b *bench) finish() report {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rep.DurationS = time.Since(b.startAt).Seconds()
	if !b.firstFrameAt.IsZero() {
		b.rep.FirstFrame = float64(b.firstFrameAt.Sub(b.startAt)) / float64(time.Millisecond)
	}

	lat := latTracker{raw: b.latRTC}
	if b.video == "ws" {
		lat = latTracker{raw: b.latWS} // absolute: frames carry capture timestamps
	} else {
		lat.normalizeRTC() // deviations around steady state; the absolute level is the floor
	}
	b.rep.Latency = lat.summary()

	// The floor anchors rtc-mode deviations: it's the absolute capture→deliver
	// latency measured on the WebSocket frames that flow while WebRTC connects.
	b.rep.Floor = 0
	if b.video == "rtc" && len(b.latWS) > 20 {
		sort.Float64s(b.latWS)
		b.rep.Floor = b.latWS[len(b.latWS)/4] // steady-state-ish (skips the startup burst)
	}
	b.rep.KFRecover = b.kfRecover
	b.rep.LossPct = b.seq.lossPct()
	if len(b.rtts) > 0 {
		b.rep.InputRTT = map[string]float64{"p50": pct(b.rtts, .5), "p95": pct(b.rtts, .95)}
	}
	b.rep.GapP95 = pct(b.gaps, .95)

	fpsVals := make([]float64, 0, len(b.rep.Series))
	var bytes int64
	for _, s := range b.rep.Series {
		fpsVals = append(fpsVals, float64(s.Frames))
		bytes += int64(s.Bytes)
	}
	if len(fpsVals) > 0 {
		b.rep.AvgFPS = avg(fpsVals)
		b.rep.FPS5 = pct(fpsVals, 0.05)
	}
	b.rep.MBps = float64(bytes) * 8 / b.rep.DurationS / 1e6
	b.rep.Blackout = b.blackoutSummary()
	return b.rep
}

func avg(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// blackoutSummary measures recovery from the -blackout window.
func (b *bench) blackoutSummary() *blackoutReport {
	if b.blackoutDur <= 0 {
		return nil
	}
	start := b.startAt.Add(b.blackoutAt)
	end := start.Add(b.blackoutDur)
	r := &blackoutReport{AtMs: b.blackoutAt.Seconds() * 1000, DurMs: b.blackoutDur.Seconds() * 1000}

	var preFrames []time.Time
	for _, t := range b.frameArrival {
		if t.After(start.Add(-3 * time.Second)) && t.Before(start) {
			preFrames = append(preFrames, t)
		}
		if t.After(end) && r.FirstFrameMs == 0 {
			r.FirstFrameMs = float64(t.Sub(end)) / float64(time.Millisecond)
		}
	}
	if len(preFrames) >= 2 {
		target := float64(len(preFrames)) / preFrames[len(preFrames)-1].Sub(preFrames[0]).Seconds() * 0.8
		for _, s := range b.rep.Series {
			at := b.startAt.Add(time.Duration(s.T+1) * time.Second)
			if at.After(end) && float64(s.Frames) >= target && r.FPS80Ms == 0 {
				r.FPS80Ms = float64(at.Sub(end)) / float64(time.Millisecond)
			}
		}
	}
	// Bitrate dip and recovery, from the server's own stats reports.
	series := append([]secStat(nil), b.rep.Series...)
	sort.Slice(series, func(i, j int) bool { return series[i].T < series[j].T })
	for _, s := range series {
		if b.startAt.Add(time.Duration(s.T)*time.Second).Before(start) && s.Br > 0 {
			r.BrBefore = s.Br
		}
	}
	nadir := int64(1 << 62)
	for _, s := range series {
		at := b.startAt.Add(time.Duration(s.T) * time.Second)
		if at.After(start) && s.Br > 0 && s.Br < nadir {
			nadir = s.Br
		}
	}
	if nadir != 1<<62 {
		r.BrNadir = nadir
	}
	for _, s := range series {
		at := b.startAt.Add(time.Duration(s.T) * time.Second)
		if at.After(end) && r.BrBefore > 0 && s.Br >= r.BrBefore*8/10 && r.BrRecoverMs == 0 {
			r.BrRecoverMs = float64(at.Sub(end)) / float64(time.Millisecond)
		}
	}
	return r
}

// testCookie gets a short test session from the running tailreach (same as
// streamtest), waiting out a just-restarted control socket.
func testCookie() string {
	dir, _ := os.UserConfigDir()
	for i := 0; i < 10; i++ {
		if c := requestTestCookie(dir); c != "" {
			return c
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ""
}

func requestTestCookie(dir string) string {
	conn, err := net.Dial("unix", filepath.Join(dir, "tailreach", "control.sock"))
	if err != nil {
		return ""
	}
	defer conn.Close()
	json.NewEncoder(conn).Encode(map[string]string{"op": "testSession", "ip": tailscaleIP()})
	var r map[string]any
	json.NewDecoder(conn).Decode(&r)
	s, _ := r["cookie"].(string)
	return s
}

// tailscaleIP is this machine's tailnet address (the Tailscale app's node, not the
// tsnet device's): the address the device can route back to.
func tailscaleIP() string {
	ip, _ := exec.Command("tailscale", "ip", "-4").Output()
	return strings.TrimSpace(string(ip))
}
