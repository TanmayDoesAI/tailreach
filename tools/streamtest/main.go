// streamtest connects to a device's /stream like a browser would and reports
// frame rate, bitrate and keyframes. Usage: go run ./tools/streamtest https://mydevices.<tailnet>.ts.net/mac/
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/coder/websocket"
)

func main() {
	page, _ := url.Parse(os.Args[1])
	u := *page
	u.Scheme = "wss"
	u.Path += "stream"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://" + page.Host}}})
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	c.SetReadLimit(64 << 20)
	c.Write(ctx, websocket.MessageText, []byte(`{"t":"start","w":2556,"h":2556,"fps":60,"maxbr":20000000}`))
	start := time.Now()
	var first time.Duration
	frames, keys, bytes := 0, 0, 0
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText {
			fmt.Println("event:", string(b))
			continue
		}
		switch b[0] {
		case 1:
			n := binary.BigEndian.Uint16(b[1:3])
			fmt.Println("config:", string(b[3:3+n]))
		case 2:
			if frames == 0 {
				first = time.Since(start)
			}
			frames++
			bytes += len(b)
			keys += int(b[1])
		}
	}
	d := time.Since(start).Seconds()
	fmt.Printf("first frame after %v · %d frames (%d key) · %.1f fps · %.2f Mbps\n", first.Round(time.Millisecond), frames, keys, float64(frames)/d, float64(bytes)*8/d/1e6)
}
