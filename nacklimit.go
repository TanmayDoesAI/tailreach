package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
)

// nackLimiter sits in front of pion's NACK responder and trims what it is asked to
// resend. Unlimited, a dip turns into a retransmission storm: the receiver asks for
// every lost packet again every 100 ms, each resend competes with new video for the
// same thin link, more is lost, and the link stays saturated after the dip has ended
// (measured: ~3000 resends a second against 50 packets of video). So:
//   - each packet is resent at most twice, and not again within one round trip;
//   - resends overall stay under a share of the video rate.
//
// A packet that can't be repaired in time is better replaced: the viewer asks for a
// keyframe when frames stay broken.
type nackLimiter struct {
	interceptor.NoOp
	rateBps *atomic.Int64 // the encoder's current target, for the resend budget

	mu     sync.Mutex
	sent   map[uint16]nackRec
	tokens float64 // resend budget, packets
	last   time.Time
}

type nackRec struct {
	n    uint8
	last time.Time
}

const (
	nackMaxResends = 2
	nackSpacing    = 80 * time.Millisecond
	nackShare      = 0.3  // resends may use this share of the video rate
	nackPktBits    = 9600 // ~1200-byte packets
	nackMinPerSec  = 20   // budget floor, packets a second
	nackForget     = 3 * time.Second
)

type nackLimiterFactory struct{ rateBps *atomic.Int64 }

func (f nackLimiterFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &nackLimiter{rateBps: f.rateBps, sent: map[uint16]nackRec{}}, nil
}

func (l *nackLimiter) BindRTCPReader(r interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attr, err := r.Read(b, a)
		if err != nil {
			return n, attr, err
		}
		if attr == nil {
			attr = interceptor.Attributes{}
		}
		pkts, err := attr.GetRTCPPackets(b[:n]) // parsed once, shared with the responder after us
		if err != nil {
			return n, attr, nil
		}
		for _, p := range pkts {
			if nk, ok := p.(*rtcp.TransportLayerNack); ok {
				l.trim(nk, time.Now())
			}
		}
		return n, attr, nil
	})
}

// trim rewrites a NACK to the packets worth resending now.
func (l *nackLimiter) trim(nk *rtcp.TransportLayerNack, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	perSec := max(float64(nackMinPerSec), float64(l.rateBps.Load())*nackShare/nackPktBits)
	if !l.last.IsZero() {
		l.tokens = min(perSec, l.tokens+now.Sub(l.last).Seconds()*perSec)
	} else {
		l.tokens = perSec / 4
	}
	l.last = now
	for seq, r := range l.sent {
		if now.Sub(r.last) > nackForget {
			delete(l.sent, seq)
		}
	}
	var keep []uint16
	for _, pair := range nk.Nacks {
		pair.Range(func(seq uint16) bool {
			r := l.sent[seq]
			if r.n >= nackMaxResends || now.Sub(r.last) < nackSpacing || l.tokens < 1 {
				return true
			}
			l.tokens--
			l.sent[seq] = nackRec{r.n + 1, now}
			keep = append(keep, seq)
			return true
		})
	}
	nk.Nacks = rtcp.NackPairsFromSequenceNumbers(keep)
}
