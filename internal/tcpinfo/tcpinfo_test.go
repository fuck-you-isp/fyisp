package tcpinfo

import (
	"io"
	"net"
	"runtime"
	"testing"
)

func TestRead(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = io.Copy(io.Discard, c)
			c.Close()
		}
	}()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	in, ok := Read(c)
	if runtime.GOOS != "linux" {
		if ok {
			t.Errorf("Read on %s = %+v, want nothing", runtime.GOOS, in)
		}
		return
	}
	if !ok || in.MSS <= 0 || in.RTT <= 0 {
		t.Errorf("Read = %+v %v", in, ok)
	}
	if _, ok := Read(&net.UDPConn{}); ok {
		t.Error("Read of a UDP conn succeeded")
	}
}
