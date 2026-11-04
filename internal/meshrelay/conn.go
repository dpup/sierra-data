package meshrelay

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// conn wraps a websocket with a single writer goroutine. gorilla allows only
// one concurrent writer, while frames for a client arrive from its bridge's
// reader goroutine and control messages from the lease ticker — so every write
// goes through the send queue, and nothing that holds the relay mutex ever
// blocks on a socket.
type conn struct {
	ws           *websocket.Conn
	send         chan outMsg
	done         chan struct{} // closed once the socket is closed
	closing      atomic.Bool
	closeOnce    sync.Once
	doneOnce     sync.Once
	pingInterval time.Duration
}

type outMsg struct {
	kind      int // websocket.TextMessage / BinaryMessage / CloseMessage
	data      []byte
	closeCode int
}

// sendQueue bounds how far a slow peer may fall behind before it is cut off.
// At the per-connection rate limit this is several seconds of frames.
const sendQueue = 256

const writeWait = 10 * time.Second

func newConn(ws *websocket.Conn, pingInterval time.Duration) *conn {
	c := &conn{ws: ws, send: make(chan outMsg, sendQueue), done: make(chan struct{}), pingInterval: pingInterval}
	ws.SetReadLimit(maxControlBytes)
	c.extendRead()
	ws.SetPongHandler(func(string) error { c.extendRead(); return nil })
	ws.SetPingHandler(func(data string) error {
		c.extendRead()
		err := ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeWait))
		if err == websocket.ErrCloseSent {
			return nil
		}
		return err
	})
	go c.writeLoop()
	return c
}

// extendRead pushes the read deadline out to three missed pongs (plus slack).
// Any inbound message, ping or pong counts as life.
func (c *conn) extendRead() {
	_ = c.ws.SetReadDeadline(time.Now().Add(3*c.pingInterval + c.pingInterval/2))
}

func (c *conn) writeLoop() {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()
	defer c.ws.Close()
	defer c.markDone()
	for {
		select {
		case m := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if m.kind == websocket.CloseMessage {
				_ = c.ws.WriteControl(websocket.CloseMessage, m.data, time.Now().Add(writeWait))
				// Give the peer a moment to read the close frame before the
				// TCP connection goes; the reader sees EOF either way.
				time.Sleep(50 * time.Millisecond)
				return
			}
			if err := c.ws.WriteMessage(m.kind, m.data); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *conn) markDone() {
	c.closing.Store(true)
	c.doneOnce.Do(func() { close(c.done) })
}

// enqueue queues a message without blocking. A full queue means the peer is
// not reading; it is closed rather than allowed to stall its sender.
func (c *conn) enqueue(m outMsg) bool {
	if c.closing.Load() {
		return false
	}
	select {
	case c.send <- m:
		return true
	case <-c.done:
		return false
	default:
		c.closeHard()
		return false
	}
}

func (c *conn) sendBinary(b []byte) bool {
	return c.enqueue(outMsg{kind: websocket.BinaryMessage, data: b})
}

func (c *conn) sendJSON(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return c.enqueue(outMsg{kind: websocket.TextMessage, data: b})
}

// close queues a close frame behind anything already queued (so a final
// lease_ending or error is delivered first) and stops further sends.
func (c *conn) close(code int, text string) {
	c.closeOnce.Do(func() {
		m := outMsg{kind: websocket.CloseMessage, data: websocket.FormatCloseMessage(code, text), closeCode: code}
		select {
		case c.send <- m:
			c.closing.Store(true)
		default:
			c.closeHard()
		}
	})
}

// closeHard drops the connection without a close handshake.
func (c *conn) closeHard() {
	c.closing.Store(true)
	c.markDone()
	c.ws.Close()
}

// rateLimiter is a token bucket: rate events/s, bursting to burst.
//
// It THROTTLES rather than disconnects: the reader waits for a token, so a
// flooding peer is slowed to the rate by TCP backpressure. Cutting the
// connection instead would kill a healthy bridge the first time its node
// dumps a long contact list or message backlog in one burst.
type rateLimiter struct {
	rate, burst, tokens float64
	last                time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// wait blocks until the next event is allowed.
func (l *rateLimiter) wait() {
	if l.rate <= 0 {
		return
	}
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	if l.tokens < 1 {
		d := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		time.Sleep(d)
		l.tokens = 1
		l.last = time.Now()
	}
	l.tokens--
}
