/*
   Package to allow go applications to immediately start
   listening on a socket, unix, tcp, udp but hold connections
   until the application has booted and is ready to accept them
*/
package listenbuffer

import "net"

// NewListenBuffer returns a listener listening on addr with the protocol.
//让 Docker Se er 立即监昕指定协议地址上的请求，但是将这些
//请求暂时先缓存下来，等 Docker Daemon 全部启动完毕之后，才让 Docker Server 开始接受
//这些请求。这样设计有一个很大的好处，那就是可以保证在 Docker Daemon 还没有完全启动
//完毕之前，接收并缓存尽可能多的用户请求。
func NewListenBuffer(proto, addr string, activate chan struct{}) (net.Listener, error) {
	wrapped, err := net.Listen(proto, addr)
	if err != nil {
		return nil, err
	}

	return &defaultListener{
		wrapped:  wrapped,
		activate: activate,
	}, nil
}

type defaultListener struct {
	wrapped  net.Listener // the real listener to wrap
	ready    bool         // is the listner ready to start accpeting connections
	activate chan struct{}
}

func (l *defaultListener) Close() error {
	return l.wrapped.Close()
}

func (l *defaultListener) Addr() net.Addr {
	return l.wrapped.Addr()
}

func (l *defaultListener) Accept() (net.Conn, error) {
	// if the listen has been told it is ready then we can go ahead and
	// start returning connections
	if l.ready {
		return l.wrapped.Accept()
	}
	<-l.activate
	l.ready = true
	return l.Accept()
}
