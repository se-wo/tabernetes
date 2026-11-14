package main

import (
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// The browser opens the data channel "ctl" with every connection: without a
// channel its offer would have no SCTP section. Both sides send short text
// messages on it.
const (
	// ctlBye goes from the agent to the tab: the agent stops now. A node
	// leaves the cluster at once, a kubectl connection gives up its token.
	ctlBye = "bye"
	// ctlByeSeen is the answer of the tab to ctlBye.
	ctlByeSeen = "bye-seen"
	// ctlClose goes from the tab to the agent: the tab removed this
	// connection (rmnode), the agent stops without a bye.
	ctlClose = "close"
)

// byeWait is how long the agent waits for ctlByeSeen. docker stop gives a
// container 10 seconds before it kills it.
const byeWait = 3 * time.Second

// ctlChannel is the part of a detached data channel that ctlLink uses.
type ctlChannel interface {
	ReadDataChannel(p []byte) (n int, isString bool, err error)
	WriteDataChannel(p []byte, isString bool) (int, error)
}

// ctlLink is the agent's end of the "ctl" channel.
type ctlLink struct {
	mu      sync.Mutex
	rw      ctlChannel
	byeSeen chan struct{}
	onClose func() // called on ctlClose
	closed  bool   // the tab sent ctlClose
}

func newCtlLink(onClose func()) *ctlLink {
	return &ctlLink{byeSeen: make(chan struct{}), onClose: onClose}
}

// attach takes the "ctl" channel that the browser opened.
func (l *ctlLink) attach(dc *webrtc.DataChannel) {
	dc.OnOpen(func() {
		rw, err := dc.Detach()
		if err != nil {
			log.Printf("ctl: %v", err)
			return
		}
		l.use(rw)
	})
}

// use starts reading the messages of rw.
func (l *ctlLink) use(rw ctlChannel) {
	l.mu.Lock()
	l.rw = rw
	l.mu.Unlock()
	go l.read(rw)
}

func (l *ctlLink) read(rw ctlChannel) {
	buf := make([]byte, 1024)
	var once sync.Once
	for {
		n, _, err := rw.ReadDataChannel(buf)
		if err != nil {
			return
		}
		switch msg := string(buf[:n]); msg {
		case ctlByeSeen:
			once.Do(func() { close(l.byeSeen) })
		case ctlClose:
			l.mu.Lock()
			l.closed = true
			l.mu.Unlock()
			l.onClose()
		default:
			log.Printf("ctl: ignoring %q", msg)
		}
	}
}

// closedByTab reports whether the tab removed this connection.
func (l *ctlLink) closedByTab() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// sayBye tells the tab that this agent stops, unless the tab ended the
// connection itself or it is gone.
func (l *ctlLink) sayBye(pc *webrtc.PeerConnection) {
	if l.closedByTab() || pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}
	if l.bye(byeWait) {
		log.Print("said bye to the browser tab")
	} else {
		log.Print("the browser tab did not answer the bye")
	}
}

// bye sends ctlBye and waits up to wait for the answer of the tab. It
// reports whether the tab answered.
func (l *ctlLink) bye(wait time.Duration) bool {
	l.mu.Lock()
	rw := l.rw
	l.mu.Unlock()
	if rw == nil {
		return false
	}
	// A write can block while SCTP waits for a peer that is gone.
	go func() { _, _ = rw.WriteDataChannel([]byte(ctlBye), true) }()
	select {
	case <-l.byeSeen:
		return true
	case <-time.After(wait):
		return false
	}
}
