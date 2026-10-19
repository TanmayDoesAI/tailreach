// streamtest connects to a device's /stream like the browser does and reports frame
// rate, bitrate and keyframes, over the WebSocket or (with -rtc) over WebRTC.
//
//	go run ./tools/streamtest [-rtc] https://mydevices.<tailnet>.ts.net/mac/
//
// It gets a short test session from the running mydevices over its local settings socket.
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
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

func main() {
	useRTC := flag.Bool("rtc", false, "receive video over WebRTC")
	secs := flag.Int("t", 6, "seconds to run")
	flag.Parse()
	page, _ := url.Parse(flag.Arg(0))
	u := *page
	u.Scheme = "wss"
	u.Path += "stream"

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*secs)*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: http.Header{
		"Origin": {"https://" + page.Host},
		"Cookie": {testCookie()},
	}})
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	c.SetReadLimit(64 << 20)
	send := func(v any) { b, _ := json.Marshal(v); c.Write(ctx, websocket.MessageText, b) }
	send(map[string]any{"t": "start", "w": 2556, "h": 2556, "fps": 60, "maxbr": 20000000, "localCursor": true})

	start := time.Now()
	var wsFrames, wsBytes, rtcPkts, rtcBytes int
	var firstWS, firstRTC time.Duration
	var pc *webrtc.PeerConnection
	if *useRTC {
		pc, _ = webrtc.NewPeerConnection(webrtc.Configuration{})
		pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			fmt.Println("track:", t.Codec().MimeType, t.Codec().SDPFmtpLine)
			for {
				p, _, err := t.ReadRTP()
				if err != nil {
					return
				}
				if rtcPkts == 0 {
					firstRTC = time.Since(start)
				}
				rtcPkts++
				rtcBytes += len(p.Payload)
			}
		})
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { fmt.Printf("webrtc %s after %v\n", s, time.Since(start).Round(time.Millisecond)) })
		offer, _ := pc.CreateOffer(nil)
		done := webrtc.GatheringCompletePromise(pc)
		pc.SetLocalDescription(offer)
		<-done
		send(map[string]any{"t": "rtc-offer", "sdp": pc.LocalDescription().SDP})
	}

	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText {
			var ev map[string]any
			json.Unmarshal(b, &ev)
			switch ev["t"] {
			case "rtc-answer":
				pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ev["sdp"].(string)})
			case "stats", "started", "rtc-failed", "error":
				fmt.Println("event:", string(b))
			}
			continue
		}
		if b[0] == 2 {
			if wsFrames == 0 {
				firstWS = time.Since(start)
			}
			wsFrames++
			wsBytes += len(b)
		} else if b[0] == 1 {
			n := binary.BigEndian.Uint16(b[1:3])
			fmt.Println("config:", string(b[3:3+n]))
		}
	}
	d := time.Since(start).Seconds()
	fmt.Printf("websocket: %d frames, first after %v, %.2f Mbps\n", wsFrames, firstWS.Round(time.Millisecond), float64(wsBytes)*8/d/1e6)
	if *useRTC {
		fmt.Printf("webrtc:    %d RTP packets, first after %v, %.2f Mbps\n", rtcPkts, firstRTC.Round(time.Millisecond), float64(rtcBytes)*8/d/1e6)
		pc.Close()
	}
}

func testCookie() string {
	dir, _ := os.UserConfigDir()
	conn, err := net.Dial("unix", filepath.Join(dir, "mydevices", "control.sock"))
	if err != nil {
		return ""
	}
	defer conn.Close()
	ip, _ := exec.Command("tailscale", "ip", "-4").Output()
	json.NewEncoder(conn).Encode(map[string]string{"op": "testSession", "ip": strings.TrimSpace(string(ip))})
	var r map[string]any
	json.NewDecoder(conn).Decode(&r)
	s, _ := r["cookie"].(string)
	return s
}
