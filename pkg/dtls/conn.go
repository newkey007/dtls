package dtls

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
)

const initialTickerInterval = time.Second
const finalTickerInternal = 90 * time.Second

// Conn represents a DTLS connection
type Conn struct {
	lock     sync.RWMutex // Internal lock (must not be public) used for Cookie/Random
	nextConn net.Conn     // Embedded Conn, typically a udpconn we read/write from

	outboundSequenceNumber uint64 // uint48

	currFlight *flight

	cipherSuite               *cipherSuite // nil if a cipherSuite hasn't been chosen
	localRandom, remoteRandom handshakeRandom
	cookie                    []byte

	decrypted    chan []byte // Decrypted Application Data, pull by calling `Read`
	workerTicker *time.Ticker
}

func createConn(isClient bool, nextConn net.Conn) *Conn {
	c := &Conn{
		nextConn:   nextConn,
		currFlight: newFlight(isClient),

		decrypted:    make(chan []byte),
		workerTicker: time.NewTicker(initialTickerInterval),
	}
	c.localRandom.populate()

	go c.readThread()
	go c.timerThread()
	return c
}

// Dial establishes a DTLS connection over an existing conn
func Dial(conn net.Conn) (*Conn, error) {
	return createConn( /*isClient*/ true, conn), nil
}

// Server listens for incoming DTLS connections
func Server(conn net.Conn) (*Conn, error) {
	return createConn( /*isClient*/ false, conn), nil
}

// Read reads data from the connection.
func (c *Conn) Read(p []byte) (n int, err error) {
	out := <-c.decrypted
	if len(p) < len(out) {
		return 0, errBufferTooSmall
	}

	copy(p, out)
	return len(p), nil
}

// Write writes len(p) bytes from p to the DTLS connection
func (c *Conn) Write(p []byte) (n int, err error) {
	return // TODO encrypt + send ApplicationData
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.nextConn.Close() // TODO Is there a better way to stop read in readThread?
	return nil
}

// Pulls from nextConn
func (c *Conn) readThread() {
	b := make([]byte, 8192)
	for {
		i, err := c.nextConn.Read(b)
		if err != nil {
			panic(err)
		}
		if err := c.handleIncoming(b[:i]); err != nil {
			panic(err)
		}
	}
}

// Handles scheduled tasks like sending ClientHello
func (c *Conn) timerThread() {
	sendPkt := func(pkt *recordLayer) {
		raw, err := pkt.marshal()
		if err != nil {
			panic(err)
		}
		c.nextConn.Write(raw)
	}

	for range c.workerTicker.C {
		switch c.currFlight.get() {
		case flight1:
			fallthrough
		case flight3:
			c.lock.RLock()
			sendPkt(&recordLayer{
				recordLayerHeader: recordLayerHeader{
					sequenceNumber:  c.outboundSequenceNumber,
					protocolVersion: protocolVersion1_2,
				},
				content: &handshake{
					// sequenceNumber and messageSequence line up, may need to be re-evaluated
					handshakeHeader: handshakeHeader{
						messageSequence: uint16(c.outboundSequenceNumber),
					},
					handshakeMessage: &handshakeMessageClientHello{
						version:            protocolVersion1_2,
						cookie:             c.cookie,
						random:             c.localRandom,
						cipherSuites:       defaultCipherSuites,
						compressionMethods: defaultCompressionMethods,
						extensions: []extension{
							&extensionSupportedGroups{
								supportedGroups: []supportedGroup{supportedGroupP256},
							},
						},
					}},
			})
			c.lock.RUnlock()
		default:
			fmt.Printf("Unhandled flight %d \n", c.currFlight.get())
		}
	}
}

func (c *Conn) handleHandshakeMessage(rawHandshake handshakeMessage) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	switch h := rawHandshake.(type) {
	case *handshakeMessageHelloVerifyRequest:
		c.cookie = append([]byte{}, h.cookie...)
		c.outboundSequenceNumber = 1
		c.currFlight.set(flight3)
	case *handshakeMessageServerHello:
		c.cipherSuite = h.cipherSuite
		c.remoteRandom = h.random
	default:
		return fmt.Errorf("Unhandled handshake %d", h.handshakeType())
	}

	return nil
}

func (c *Conn) handleIncoming(buf []byte) error {
	pkts, err := unpackDatagram(buf)
	if err != nil {
		return err
	}

	for _, p := range pkts {
		r := &recordLayer{}
		if err := r.unmarshal(p); err != nil {
			return err
		}

		switch content := r.content.(type) {
		case *alert:
			return fmt.Errorf(spew.Sdump(content))
		case *handshake:
			if err := c.handleHandshakeMessage(content.handshakeMessage); err != nil {
				return err
			}
		default:
			return fmt.Errorf("Unhandled contentType %d", content.contentType())
		}
	}
	return nil
}
