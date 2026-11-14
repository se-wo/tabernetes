// Proof of concept for phase 0, item 10 of ../../wasi-sockets.md (ND3): an
// unordered WebRTC data channel "udp" without retransmits between the
// browser and pion, the WebRTC of the node agent. Each side opens one such
// channel, so each side sees the settings of a channel that the other side
// made. A message of 65,521 bytes (a header of 14 bytes and a datagram of
// 65,507 bytes) goes in the two directions on each channel.
//
// It serves a page on 127.0.0.1:<port>; run.sh webrtc opens it in headless
// Chromium. The page sends its offer to /offer and its results to /result.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const size = 65521

var (
	mu      sync.Mutex
	results []string
	done    = make(chan struct{})
)

func logf(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	s := fmt.Sprintf(format, a...)
	results = append(results, s)
	fmt.Println(s)
}

func describe(dc *webrtc.DataChannel) string {
	mr := "none"
	if v := dc.MaxRetransmits(); v != nil {
		mr = fmt.Sprint(*v)
	}
	return fmt.Sprintf("label %q ordered %v maxRetransmits %s", dc.Label(), dc.Ordered(), mr)
}

func use(dc *webrtc.DataChannel, who string) {
	dc.OnOpen(func() {
		logf("pion: channel %s open: %s", who, describe(dc))
		msg := make([]byte, size)
		msg[0] = 1
		if err := dc.Send(msg); err != nil {
			logf("pion: send %d bytes on %s: %v", size, who, err)
		}
		big := make([]byte, 65537)
		if err := dc.Send(big); err != nil {
			logf("pion: send 65,537 bytes on %s: %v", who, err)
		}
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		logf("pion: received %d bytes on %s", len(m.Data), who)
	})
}

func main() {
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, page)
	})
	mux.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		var offer webrtc.SessionDescription
		if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		pc, err := api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		pc.OnDataChannel(func(dc *webrtc.DataChannel) { use(dc, "from the browser") })
		// The channel that pion makes, as the node agent would.
		ordered, zero := false, uint16(0)
		dc, err := pc.CreateDataChannel("udp", &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &zero})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		use(dc, "from pion")
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			if s == webrtc.PeerConnectionStateConnected {
				if sctp := pc.SCTP(); sctp != nil {
					logf("pion: remote max-message-size %d", sctp.GetCapabilities().MaxMessageSize)
				}
			}
		})
		if err := pc.SetRemoteDescription(offer); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		answer, _ := pc.CreateAnswer(nil)
		gathered := webrtc.GatheringCompletePromise(pc)
		pc.SetLocalDescription(answer)
		<-gathered
		json.NewEncoder(w).Encode(pc.LocalDescription())
	})
	mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		logf("browser: %s", b)
		if string(b) == "done" {
			close(done)
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Printf("URL http://%s/\n", ln.Addr())
	go http.Serve(ln, mux)
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		fmt.Println("timeout")
		os.Exit(1)
	}
	time.Sleep(500 * time.Millisecond)
}

const page = `<!doctype html><meta charset="utf-8"><script type="module">
const log = (s) => fetch("/result", { method: "POST", body: s });
const pc = new RTCPeerConnection();
const desc = (dc) => "label " + dc.label + " ordered " + dc.ordered + " maxRetransmits " + dc.maxRetransmits;
let got = 0;
function use(dc, who) {
  dc.binaryType = "arraybuffer";
  dc.onopen = () => {
    log("channel " + who + " open: " + desc(dc) + "; sctp.maxMessageSize " + pc.sctp?.maxMessageSize);
    dc.send(new Uint8Array(65521));
    try { dc.send(new Uint8Array(65537)); } catch (e) { log("send 65,537 bytes on " + who + ": " + e.name + ": " + e.message); }
  };
  dc.onmessage = (m) => { log("received " + m.data.byteLength + " bytes on " + who); if (++got === 2) setTimeout(() => log("done"), 1000); };
}
pc.ondatachannel = (e) => use(e.channel, "from pion");
use(pc.createDataChannel("udp", { ordered: false, maxRetransmits: 0 }), "from the browser");
await pc.setLocalDescription(await pc.createOffer());
// Headless Chrome does not always end the gathering: wait at most 1 s.
await new Promise((r) => { setTimeout(r, 1000); pc.onicegatheringstatechange = () => pc.iceGatheringState === "complete" && r(); });
const answer = await (await fetch("/offer", { method: "POST", body: JSON.stringify(pc.localDescription) })).json();
await pc.setRemoteDescription(answer);
</script>`
