package main

import (
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// WebRTC video: the same hardware H.264, but carried the way AirPlay and FaceTime carry
// video. UDP (a lost packet is resent or skipped, never stalls everything behind it),
// a jitter buffer on the phone, and Google Congestion Control steering the encoder.
//
// Input rides the same connection, on two data channels the browser opens:
//   "input": ordered and reliable (clicks, keys, scrolls, trackpad moves)
//   "move":  unordered, never resent (pointer positions, and the Mac's cursor reports
//            back): a lost one is simply replaced by the next, so nothing ever waits.
// Over UDP, a lost packet holds up nothing else, unlike the WebSocket's TCP.
//
// ICE only ever offers this node's Tailscale address, so media and input flow over the
// tailnet and nowhere else. The WebSocket stays up for control, and carries video and
// input too if WebRTC can't connect.

const rtcPort = "50050"

var rtcMux struct {
	sync.Mutex
	mux ice.UDPMux
}

// iceMux is one UDP socket on our tailnet IP, shared by every peer connection.
func (a *app) iceMux() (ice.UDPMux, error) {
	rtcMux.Lock()
	defer rtcMux.Unlock()
	if rtcMux.mux != nil {
		return rtcMux.mux, nil
	}
	if a.tsIP4 == "" {
		return nil, errors.New("no tailnet IPv4 address")
	}
	pc, err := a.srv.ListenPacket("udp", net.JoinHostPort(a.tsIP4, rtcPort))
	if err != nil {
		return nil, err
	}
	rtcMux.mux = webrtc.NewICEUDPMux(nil, pc)
	return rtcMux.mux, nil
}

type rtcSession struct {
	pc        *webrtc.PeerConnection
	track     *webrtc.TrackLocalStaticRTP
	connected atomic.Bool
	est       atomic.Value // cc.BandwidthEstimator
	sentBytes atomic.Int64
	move      atomic.Pointer[webrtc.DataChannel]

	mu      sync.Mutex
	sps     [][]byte
	pps     [][]byte
	pkt     rtp.Packetizer
	basePTS uint64 // µs of the first frame
	baseRTP uint32
	lastRTP uint32
	started bool
	lastKF  time.Time
}

// Keyframes are expensive; heavy loss can ask for one per packet. One per this long is plenty.
const keyframeEvery = 300 * time.Millisecond

// newRTC answers the browser's offer. onKeyframe is called when the browser asks for one;
// onInput for each command arriving on a data channel, with a way to answer on it.
func (a *app) newRTC(offer string, maxBitrate int, onKeyframe, onConnected, onFailed func(),
	onInput func(msg []byte, reply func([]byte))) (*rtcSession, string, error) {
	mux, err := a.iceMux()
	if err != nil {
		return nil, "", err
	}

	m := &webrtc.MediaEngine{}
	feedback := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}, {Type: "transport-cc"}}
	for pt, profile := range map[webrtc.PayloadType]string{102: "640c1f", 106: "42e01f"} {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: 90000, RTCPFeedback: feedback,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile,
			},
			PayloadType: pt,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, "", err
		}
	}

	s := &rtcSession{}
	reg := &interceptor.Registry{}
	ccf, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(6_000_000),
			gcc.SendSideBWEMinBitrate(minBitrate),
			gcc.SendSideBWEMaxBitrate(maxBitrate),
			// No pacer queue: each frame goes out the moment it's encoded. A pacer queue can
			// grow without limit when the estimate drops, adding hidden delay; the encoder's
			// own rate control (steered from the estimate) keeps us within the path instead.
			gcc.SendSideBWEPacer(gcc.NewNoOpPacer()))
	})
	if err != nil {
		return nil, "", err
	}
	ccf.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { s.est.Store(e) })
	reg.Add(ccf)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, reg); err != nil {
		return nil, "", err
	}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil { // NACK retransmits, RTCP reports
		return nil, "", err
	}

	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", err
	}
	s.pc = pc
	s.track, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "screen", "mydevices")
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	sender, err := pc.AddTrack(s.track)
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	// SSRC and payload type are filled in per connection by the track.
	s.pkt = rtp.NewPacketizer(1200, 0, 0, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
	s.baseRTP = randUint32()
	go func() { // keyframe requests from the browser (decoder reset, heavy loss)
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				switch p.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					s.mu.Lock()
					due := time.Since(s.lastKF) >= keyframeEvery
					if due {
						s.lastKF = time.Now()
					}
					s.mu.Unlock()
					if due {
						onKeyframe()
					}
				}
			}
		}
	}()
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		label := dc.Label()
		if label != "input" && label != "move" {
			dc.Close()
			return
		}
		if label == "move" {
			dc.OnOpen(func() { s.move.Store(dc) })
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				onInput(m.Data, func(b []byte) { dc.SendText(string(b)) })
			}
		})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateConnected:
			if !s.connected.Swap(true) {
				onConnected()
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if s.connected.Swap(false) {
				onFailed()
			}
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		pc.Close()
		return nil, "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return nil, "", err
	}
	select {
	case <-gathered:
	case <-time.After(3 * time.Second):
	}
	return s, pc.LocalDescription().SDP, nil
}

// targetBitrate is what congestion control thinks the path can carry right now.
func (s *rtcSession) targetBitrate() int {
	if e, ok := s.est.Load().(cc.BandwidthEstimator); ok {
		return e.GetTargetBitrate()
	}
	return 0
}

// setConfig takes the helper's decoder config message ([1][u16 len][json][avcC]) and keeps SPS/PPS.
func (s *rtcSession) setConfig(msg []byte) {
	if len(msg) < 3 {
		return
	}
	jl := int(binary.BigEndian.Uint16(msg[1:3]))
	if len(msg) < 3+jl {
		return
	}
	sps, pps := parseAvcC(msg[3+jl:])
	s.mu.Lock()
	s.sps, s.pps = sps, pps
	s.mu.Unlock()
}

// writeFrame takes a helper video message ([2][u8 key][u64 µs][AVCC NALs]) and sends it.
// Each frame's RTP timestamp is its own capture time, so the browser's jitter buffer
// sees true timing, including long still stretches between bursts of movement.
func (s *rtcSession) writeFrame(msg []byte) error {
	if len(msg) < 10 {
		return nil
	}
	key := msg[1] == 1
	pts := binary.BigEndian.Uint64(msg[2:10])
	s.mu.Lock()
	if !s.started {
		s.started, s.basePTS = true, pts
	}
	ts := s.baseRTP + uint32((pts-s.basePTS)*90/1000) // wraps naturally
	if pts < s.basePTS || (ts-s.lastRTP > 1<<31 && s.lastRTP != 0) {
		ts = s.lastRTP + 1 // never step backwards
	}
	s.lastRTP = ts
	var annexB []byte
	if key { // decoders need SPS/PPS in front of every keyframe
		s.lastKF = time.Now()
		for _, n := range append(append([][]byte{}, s.sps...), s.pps...) {
			annexB = append(append(annexB, 0, 0, 0, 1), n...)
		}
	}
	annexB = appendAnnexB(annexB, msg[10:])
	pkts := s.pkt.Packetize(annexB, 0)
	s.mu.Unlock()
	s.sentBytes.Add(int64(len(annexB)))
	for _, p := range pkts {
		p.Timestamp = ts
		if err := s.track.WriteRTP(p); err != nil {
			return err
		}
	}
	return nil
}

// sendMove sends a message the newest copy of which is all that matters (a cursor report)
// on the unreliable channel. False if that channel isn't open.
func (s *rtcSession) sendMove(msg []byte) bool {
	dc := s.move.Load()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	if dc.BufferedAmount() < 32<<10 { // backed up: skip this one, a newer report follows
		dc.SendText(string(msg))
	}
	return true
}

func randUint32() uint32 {
	var b [4]byte
	crand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

func (s *rtcSession) close() { s.pc.Close() }

// appendAnnexB converts 4-byte length-prefixed NAL units to start-code-prefixed ones.
func appendAnnexB(dst, avcc []byte) []byte {
	for len(avcc) >= 4 {
		n := int(binary.BigEndian.Uint32(avcc[:4]))
		avcc = avcc[4:]
		if n > len(avcc) {
			break
		}
		dst = append(append(dst, 0, 0, 0, 1), avcc[:n]...)
		avcc = avcc[n:]
	}
	return dst
}

// parseAvcC pulls the SPS and PPS out of an AVCDecoderConfigurationRecord.
func parseAvcC(b []byte) (sps, pps [][]byte) {
	if len(b) < 7 {
		return
	}
	i := 5
	nSPS := int(b[i] & 0x1f)
	i++
	for k := 0; k < nSPS && i+2 <= len(b); k++ {
		n := int(binary.BigEndian.Uint16(b[i:]))
		i += 2
		if i+n > len(b) {
			return
		}
		sps = append(sps, append([]byte(nil), b[i:i+n]...))
		i += n
	}
	if i >= len(b) {
		return
	}
	nPPS := int(b[i])
	i++
	for k := 0; k < nPPS && i+2 <= len(b); k++ {
		n := int(binary.BigEndian.Uint16(b[i:]))
		i += 2
		if i+n > len(b) {
			return
		}
		pps = append(pps, append([]byte(nil), b[i:i+n]...))
		i += n
	}
	return
}

func jsonMsg(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
