package egress

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type closeWriteBarrier struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *closeWriteBarrier) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Conn.Write(p)
}

type closeHijacker struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *closeHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestCONNECTCloseBeforeRegistrationRejectsTunnel(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := listener.Accept(); accepted <- conn }()
	address := listener.Addr().(*net.TCPAddr)
	channel, err := listenProxyChannel(NewStaticGate(Target{Host: "127.0.0.1", Protocol: "https", Port: uint16(address.Port), Methods: []string{"CONNECT"}, AllowPrivate: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer channel.close()
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	barrier := &closeWriteBarrier{Conn: remote, entered: make(chan struct{}), release: make(chan struct{})}
	request := httptest.NewRequest(http.MethodConnect, "http://"+listener.Addr().String(), nil)
	request = request.WithContext(context.Background())
	request.Host = listener.Addr().String()
	served := make(chan struct{})
	go func() {
		channel.serveConnect(&closeHijacker{ResponseRecorder: httptest.NewRecorder(), conn: barrier}, request)
		close(served)
	}()
	upstream := <-accepted
	if upstream == nil {
		t.Fatal("no upstream")
	}
	defer upstream.Close()
	<-barrier.entered // CONNECT is hijacked, but its 200 response has not yet flushed.
	if err := channel.close(); err != nil {
		t.Fatal(err)
	}
	close(barrier.release)
	_ = local.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(local)
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("CONNECT acknowledged after channel close returned")
	}
	<-served
	_ = upstream.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := upstream.Read(make([]byte, 1)); err == nil {
		t.Fatal("upstream connection survived close")
	}
}
