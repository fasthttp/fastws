package fastws

import (
	"bufio"
	"io"
	"net"
	"sync"
)

// Mode is the mode in which the bytes are sended.
type Mode uint8

const (
	ModeText Mode = iota
	ModeBinary
)

var connPool sync.Pool

// Conn represents websocket connection handler.
//
// This handler is compatible with io.Reader, io.ReaderFrom, io.Writer, io.WriterTo
type Conn struct {
	sync.Mutex

	c net.Conn

	wpool sync.Pool
	rpool sync.Pool

	server bool
	closed bool

	// extra bytes
	extra []byte

	// Mode indicates Write and Read default mode.
	Mode Mode
}

func acquireConn(c net.Conn) (conn *Conn) {
	ci := connPool.Get()
	if ci != nil {
		conn = ci.(*Conn)
	} else {
		conn = &Conn{}
	}
	conn.Reset(c)
	return conn
}

func (conn *Conn) acquireReader() (br *bufio.Reader) {
	r := conn.rpool.Get()
	if r == nil {
		br = bufio.NewReader(conn.c)
	} else {
		br = r.(*bufio.Reader)
		br.Reset(conn.c)
	}
	return br
}

func (conn *Conn) acquireWriter() (bw *bufio.Writer) {
	w := conn.wpool.Get()
	if w == nil {
		bw = bufio.NewWriter(conn.c)
	} else {
		bw = w.(*bufio.Writer)
		bw.Reset(conn.c)
	}
	return bw
}

func (conn *Conn) releaseReader(br *bufio.Reader) {
	conn.rpool.Put(br)
}

func (conn *Conn) releaseWriter(bw *bufio.Writer) {
	conn.wpool.Put(bw)
}

// Reset resets conn values setting c as default connection endpoint.
func (conn *Conn) Reset(c net.Conn) {
	if conn.c != nil {
		conn.Close()
	}
	conn.closed = false
	conn.extra = conn.extra[:0]
	conn.c = c
}

// Write writes b using conn.Mode as default.
func (conn *Conn) Write(b []byte) (int, error) {
	return conn.write(conn.Mode, b)
}

// Read fills b using conn.Mode as default.
func (conn *Conn) Read(b []byte) (int, error) {
	return conn.read(conn.Mode, b)
}

// WriteMode writes b to conn using mode.
func (conn *Conn) WriteMode(mode Mode, b []byte) (int, error) {
	return conn.write(mode, b)
}

// ReadMode fills b reading from conn using mode.
//
// Default mode in Read is conn.Mode
func (conn *Conn) ReadMode(mode Mode, b []byte) (int, error) {
	return conn.read(mode, b)
}

// WriteFrame writes fr to the connection endpoint.
func (conn *Conn) WriteFrame(fr *Frame) (int, error) {
	bw := conn.acquireWriter()
	nn, err := fr.WriteTo(bw)
	if err == nil {
		err = bw.Flush()
	}
	conn.releaseWriter(bw)
	return int(nn), err
}

// ReadFrame fills fr with the next connection frame.
func (conn *Conn) ReadFrame(fr *Frame) (int, error) {
	br := conn.acquireReader()
	nn, err := fr.ReadFrom(br)
	conn.releaseReader(br)
	return int(nn), err
}

// NextFrame reads next connection frame and returns if there were no error.
//
// If NextFrame does not return any error do not forget to ReleaseFrame(fr)
func (conn *Conn) NextFrame() (fr *Frame, err error) {
	br := conn.acquireReader()
	fr = AcquireFrame()
	_, err = fr.ReadFrom(br)
	conn.releaseReader(br)
	if err != nil {
		ReleaseFrame(fr)
		fr = nil
	}
	return fr, err
}

// TODO: Add timeout
func (conn *Conn) write(mode Mode, b []byte) (n int, err error) {
	if conn.checkClose() {
		err = io.EOF
		return
	}

	var nn uint64
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	// TODO: Apply continuation frames if b is large.
	fr.SetFin()
	fr.SetText()
	fr.SetPayload(b)
	if !conn.server {
		fr.Mask()
	}

	bw := conn.acquireWriter()
	defer conn.releaseWriter(bw)

	nn, err = fr.WriteTo(bw)
	n = int(nn)
	if err == nil {
		err = bw.Flush()
	}

	return
}

// TODO: Add timeout
func (conn *Conn) read(mode Mode, b []byte) (n int, err error) {
	if conn.checkClose() {
		err = io.EOF
		return
	}

	var nn uint64
	var c int
	max := len(b)
	// TODO: Check concurrency
	if n = len(conn.extra); n > 0 {
		n = copy(b, conn.extra)
		// TODO: Check allocations
		conn.extra = conn.extra[n:]
		if n == max {
			return
		}
	}

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	br := conn.acquireReader()
	defer conn.releaseReader(br)

	for n < max {
		nn, err = fr.ReadFrom(br)
		if err != nil {
			break
		}
		if conn.server && fr.IsMasked() {
			fr.Unmask()
		}

		c = copy(b[n:], fr.payload)
		n += c

		if fr.IsFin() {
			break
		}
		fr.Reset()
	}
	if c < int(nn) {
		conn.extra = append(conn.extra[:0], fr.payload[c:]...)
	}
	return
}

// Close closes the connection sending CodeClose and closing the descriptor.
func (conn *Conn) Close() error {
	if conn.checkClose() {
		return nil
	}

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetFin()
	fr.SetClose()
	_, err := conn.WriteFrame(fr)
	if err == nil {
		err = conn.c.Close()
		if err == nil {
			conn.c = nil
			conn.Lock()
			conn.closed = true
			conn.Unlock()
		}
	}
	return err
}

func (conn *Conn) checkClose() (closed bool) {
	conn.Lock()
	closed = conn.closed
	conn.Unlock()
	return
}

// TODO: https://tools.ietf.org/html/rfc6455#section-5.4
