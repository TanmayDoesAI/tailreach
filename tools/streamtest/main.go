// streamtest connects to a device's /stream like the browser does and reports frame
// rate, bitrate and keyframes, over the WebSocket or (with -rtc) over WebRTC. With -rtc
// it also opens the input data channels and reports round trips and cursor reports on them.
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
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

func main() {
	useRTC := flag.Bool("rtc", false, "receive video over WebRTC")
	secs := flag.Int("t", 6, "seconds to run")
	nudge := flag.Bool("nudge", false, "move the Mac's cursor 40 points right and back, to check input works")
	display := flag.Int("display", 0, "display ID to show (0 = main; the IDs are printed in the displays event)")
	useHEVC := flag.Bool("hevc", false, "ask for HEVC video instead of H.264")
	upload := flag.String("upload", "", "send this file to the Mac's Downloads instead, as a viewer would")
	flag.Parse()
	page, _ := url.Parse(flag.Arg(0))
	if *upload != "" {
		sendFile(page, *upload)
		return
	}
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
	var inputDC atomic.Pointer[webrtc.DataChannel]
	send := func(v any) {
		b, _ := json.Marshal(v)
		if dc := inputDC.Load(); dc != nil && v.(map[string]any)["t"] != "start" {
			dc.SendText(string(b))
			return
		}
		c.Write(ctx, websocket.MessageText, b)
	}
	codec := "h264"
	if *useHEVC {
		codec = "hevc"
	}
	send(map[string]any{"t": "start", "w": 2556, "h": 2556, "fps": 60, "maxbr": 20000000, "localCursor": true, "display": *display, "codec": codec})

	send(map[string]any{"t": "diag"})
	if *nudge {
		go func() {
			time.Sleep(time.Second)
			send(map[string]any{"t": "rel", "dx": 40, "dy": 0})
			time.Sleep(500 * time.Millisecond)
			send(map[string]any{"t": "rel", "dx": -40, "dy": 0})
		}()
	}
	start := time.Now()
	var wsFrames, wsBytes, rtcPkts, rtcBytes int
	var firstWS, firstRTC time.Duration
	var dcPongs, dcCursors atomic.Int64
	var dcRTT atomic.Int64 // µs, latest
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
		in, _ := pc.CreateDataChannel("input", nil)
		mv, _ := pc.CreateDataChannel("move", &webrtc.DataChannelInit{Ordered: new(bool), MaxRetransmits: new(uint16)})
		in.OnOpen(func() {
			fmt.Printf("data channels open after %v\n", time.Since(start).Round(time.Millisecond))
			inputDC.Store(in)
			go func() {
				for ctx.Err() == nil {
					send(map[string]any{"t": "ping", "at": time.Now().UnixMicro()})
					time.Sleep(500 * time.Millisecond)
				}
			}()
		})
		in.OnMessage(func(m webrtc.DataChannelMessage) {
			var ev map[string]any
			if json.Unmarshal(m.Data, &ev) == nil && ev["t"] == "pong" {
				dcPongs.Add(1)
				dcRTT.Store(time.Now().UnixMicro() - int64(ev["at"].(float64)))
			}
		})
		mv.OnMessage(func(m webrtc.DataChannelMessage) {
			dcCursors.Add(1)
			if *nudge {
				fmt.Printf("cursor (move channel) %s\n", m.Data)
			}
		})
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
			case "cursor":
				if *nudge {
					fmt.Printf("cursor x=%.4f\n", ev["x"])
				}
			case "stats", "started", "rtc-failed", "error", "warn", "diag", "displays":
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
		fmt.Printf("input dc:  %d pongs, last round trip %v; %d cursor reports on the move channel\n",
			dcPongs.Load(), time.Duration(dcRTT.Load())*time.Microsecond, dcCursors.Load())
		pc.Close()
	}
}

func sendFile(page *url.URL, path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer f.Close()
	u := *page
	u.Path += "upload"
	u.RawQuery = url.Values{"name": {filepath.Base(path)}}.Encode()
	req, _ := http.NewRequest("POST", u.String(), f)
	req.Header.Set("Origin", "https://"+page.Host)
	req.Header.Set("Cookie", testCookie())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Println(resp.Status, strings.TrimSpace(string(b)))
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
