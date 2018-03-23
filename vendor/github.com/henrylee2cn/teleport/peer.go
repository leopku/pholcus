// Copyright 2015-2017 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/henrylee2cn/goutil"
	"github.com/henrylee2cn/goutil/coarsetime"
	"github.com/henrylee2cn/goutil/errors"
	"github.com/henrylee2cn/teleport/codec"
	"github.com/henrylee2cn/teleport/socket"
)

type (
	// BasePeer peer with the common method set
	BasePeer interface {
		// Close closes peer.
		Close() (err error)
		// CountSession returns the number of sessions.
		CountSession() int
		// Dial connects with the peer of the destination address.
		Dial(addr string, protoFunc ...socket.ProtoFunc) (Session, *Rerror)
		// DialContext connects with the peer of the destination address, using the provided context.
		DialContext(ctx context.Context, addr string, protoFunc ...socket.ProtoFunc) (Session, *Rerror)
		// GetSession gets the session by id.
		GetSession(sessionId string) (Session, bool)
		// RangeSession ranges all sessions. If fn returns false, stop traversing.
		RangeSession(fn func(sess Session) bool)
		// ServeConn serves the connection and returns a session.
		ServeConn(conn net.Conn, protoFunc ...socket.ProtoFunc) (Session, error)
		// SetTlsConfig sets the TLS config.
		SetTlsConfig(tlsConfig *tls.Config)
		// SetTlsConfigFromFile sets the TLS config from file.
		SetTlsConfigFromFile(tlsCertFile, tlsKeyFile string) error
		// TlsConfig returns the TLS config.
		TlsConfig() *tls.Config
	}
	// EarlyPeer the communication peer that has just been created
	EarlyPeer interface {
		BasePeer
		// Router returns the root router of pull or push handlers.
		Router() *Router
		// SubRoute adds handler group.
		SubRoute(pathPrefix string, plugin ...Plugin) *SubRouter
		// RoutePull registers PULL handlers, and returns the paths.
		RoutePull(ctrlStruct interface{}, plugin ...Plugin) []string
		// RoutePullFunc registers PULL handler, and returns the path.
		RoutePullFunc(pullHandleFunc interface{}, plugin ...Plugin) string
		// RoutePush registers PUSH handlers, and returns the paths.
		RoutePush(ctrlStruct interface{}, plugin ...Plugin) []string
		// RoutePushFunc registers PUSH handler, and returns the path.
		RoutePushFunc(pushHandleFunc interface{}, plugin ...Plugin) string
		// SetUnknownPull sets the default handler, which is called when no handler for PULL is found.
		SetUnknownPull(fn func(UnknownPullCtx) (interface{}, *Rerror), plugin ...Plugin)
		// SetUnknownPush sets the default handler, which is called when no handler for PUSH is found.
		SetUnknownPush(fn func(UnknownPushCtx) *Rerror, plugin ...Plugin)
	}
	// Peer the communication peer which is server or client role
	Peer interface {
		EarlyPeer
		// Listen turns on the listening service.
		Listen(protoFunc ...socket.ProtoFunc) error
	}
	peer struct {
		router            *Router
		pluginContainer   *PluginContainer
		sessHub           *SessionHub
		closeCh           chan struct{}
		freeContext       *readHandleCtx
		ctxLock           sync.Mutex
		defaultSessionAge time.Duration // Default session max age, if less than or equal to 0, no time limit
		defaultContextAge time.Duration // Default PULL or PUSH context max age, if less than or equal to 0, no time limit
		tlsConfig         *tls.Config
		slowCometDuration time.Duration
		defaultBodyCodec  byte
		printBody         bool
		countTime         bool
		timeNow           func() time.Time
		timeSince         func(time.Time) time.Duration
		mu                sync.Mutex

		network string

		// only for client role
		defaultDialTimeout time.Duration
		redialTimes        int32

		// only for server role
		listenAddr string
		listen     net.Listener
	}
)

var (
	_ BasePeer  = new(peer)
	_ EarlyPeer = new(peer)
	_ Peer      = new(peer)
)

// NewPeer creates a new peer.
func NewPeer(cfg PeerConfig, plugin ...Plugin) Peer {
	doPrintPid()
	pluginContainer := newPluginContainer()
	pluginContainer.AppendRight(plugin...)
	pluginContainer.PreNewPeer(&cfg)
	if err := cfg.check(); err != nil {
		Fatalf("%v", err)
	}
	var p = &peer{
		router:             newRouter("/", pluginContainer),
		pluginContainer:    pluginContainer,
		sessHub:            newSessionHub(),
		defaultSessionAge:  cfg.DefaultSessionAge,
		defaultContextAge:  cfg.DefaultContextAge,
		closeCh:            make(chan struct{}),
		slowCometDuration:  cfg.slowCometDuration,
		defaultDialTimeout: cfg.DefaultDialTimeout,
		network:            cfg.Network,
		listenAddr:         cfg.ListenAddress,
		printBody:          cfg.PrintBody,
		countTime:          cfg.CountTime,
		redialTimes:        cfg.RedialTimes,
	}
	if c, err := codec.GetByName(cfg.DefaultBodyCodec); err != nil {
		Fatalf("%v", err)
	} else {
		p.defaultBodyCodec = c.Id()
	}
	if p.countTime {
		p.timeNow = time.Now
		p.timeSince = time.Since
	} else {
		t0 := time.Time{}
		p.timeNow = func() time.Time { return t0 }
		p.timeSince = func(time.Time) time.Duration { return 0 }
	}
	addPeer(p)
	p.pluginContainer.PostNewPeer(p)
	return p
}

// TlsConfig returns the TLS config.
func (p *peer) TlsConfig() *tls.Config {
	return p.tlsConfig
}

// SetTlsConfig sets the TLS config.
func (p *peer) SetTlsConfig(tlsConfig *tls.Config) {
	p.tlsConfig = tlsConfig
}

// SetTlsConfigFromFile sets the TLS config from file.
func (p *peer) SetTlsConfigFromFile(tlsCertFile, tlsKeyFile string) error {
	var err error
	p.tlsConfig, err = NewTlsConfigFromFile(tlsCertFile, tlsKeyFile)
	return err
}

// GetSession gets the session by id.
func (p *peer) GetSession(sessionId string) (Session, bool) {
	return p.sessHub.Get(sessionId)
}

// RangeSession ranges all sessions.
// If fn returns false, stop traversing.
func (p *peer) RangeSession(fn func(sess Session) bool) {
	p.sessHub.sessions.Range(func(key, value interface{}) bool {
		return fn(value.(*session))
	})
}

// CountSession returns the number of sessions.
func (p *peer) CountSession() int {
	return p.sessHub.sessions.Len()
}

// Dial connects with the peer of the destination address.
func (p *peer) Dial(addr string, protoFunc ...socket.ProtoFunc) (Session, *Rerror) {
	return p.newSessionForClient(func() (net.Conn, error) {
		return net.DialTimeout(p.network, addr, p.defaultDialTimeout)
	}, addr, protoFunc)
}

// DialContext connects with the peer of the destination address,
// using the provided context.
func (p *peer) DialContext(ctx context.Context, addr string, protoFunc ...socket.ProtoFunc) (Session, *Rerror) {
	return p.newSessionForClient(func() (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, p.network, addr)
	}, addr, protoFunc)
}

func (p *peer) newSessionForClient(dialFunc func() (net.Conn, error), addr string, protoFuncs []socket.ProtoFunc) (*session, *Rerror) {
	var conn, dialErr = dialFunc()
	if dialErr != nil {
		rerr := rerrDialFailed.Copy()
		rerr.Detail = dialErr.Error()
		return nil, rerr
	}
	if p.tlsConfig != nil {
		conn = tls.Client(conn, p.tlsConfig)
	}
	var sess = newSession(p, conn, protoFuncs)

	// create redial func
	if p.redialTimes > 0 {
		sess.redialForClientLocked = func(oldConn net.Conn) bool {
			if oldConn != sess.conn {
				return true
			}
			var err error
			for i := p.redialTimes; i > 0; i-- {
				err = p.renewSessionForClient(sess, dialFunc, addr, protoFuncs)
				if err == nil {
					return true
				}
				// if i > 1 {
				// 	Warnf("redial fail (network:%s, addr:%s, id:%s): %s", p.network, sess.RemoteIp(), sess.Id(), err.Error())
				// 	// Debug:
				// 	time.Sleep(5e9)
				// }
			}
			if err != nil {
				Errorf("redial fail (network:%s, addr:%s, id:%s): %s", p.network, sess.RemoteIp(), sess.Id(), err.Error())
			}
			return false
		}
	}

	sess.socket.SetId(sess.LocalIp())
	if rerr := p.pluginContainer.PostDial(sess); rerr != nil {
		sess.Close()
		return nil, rerr
	}
	AnywayGo(sess.startReadAndHandle)
	p.sessHub.Set(sess)
	Infof("dial ok (network:%s, addr:%s, id:%s)", p.network, sess.RemoteIp(), sess.Id())
	return sess, nil
}

func (p *peer) renewSessionForClient(sess *session, dialFunc func() (net.Conn, error), addr string, protoFuncs []socket.ProtoFunc) error {
	var conn, dialErr = dialFunc()
	if dialErr != nil {
		return dialErr
	}
	if p.tlsConfig != nil {
		conn = tls.Client(conn, p.tlsConfig)
	}
	oldIp := sess.LocalIp()
	oldId := sess.Id()
	sess.conn = conn
	sess.socket.Reset(conn, protoFuncs...)
	if oldIp == oldId {
		sess.socket.SetId(sess.LocalIp())
	} else {
		sess.socket.SetId(oldId)
	}
	if rerr := p.pluginContainer.PostDial(sess); rerr != nil {
		sess.Close()
		return rerr.ToError()
	}
	atomic.StoreInt32(&sess.status, statusOk)
	AnywayGo(sess.startReadAndHandle)
	p.sessHub.Set(sess)
	Infof("redial ok (network:%s, addr:%s, id:%s)", p.network, sess.RemoteIp(), sess.Id())
	return nil
}

// ErrListenClosed listener is closed error.
var ErrListenClosed = errors.New("listener is closed")

// Listen turns on the listening service.
func (p *peer) Listen(protoFunc ...socket.ProtoFunc) error {
	if len(p.listenAddr) == 0 {
		Fatalf("listenAddress can not be empty")
	}
	lis, err := NewInheritListener(p.network, p.listenAddr, p.tlsConfig)
	if err != nil {
		Fatalf("%v", err)
	}
	defer lis.Close()
	p.listen = lis

	network := lis.Addr().Network()
	addr := lis.Addr().String()
	Printf("listen ok (network:%s, addr:%s)", network, addr)

	p.pluginContainer.PostListen(lis.Addr())

	var (
		tempDelay time.Duration // how long to sleep on accept failure
		closeCh   = p.closeCh
	)
	for {
		conn, e := lis.Accept()
		if e != nil {
			select {
			case <-closeCh:
				return ErrListenClosed
			default:
			}
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}

				Tracef("accept error: %s; retrying in %v", e.Error(), tempDelay)

				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0
		AnywayGo(func() {
			if c, ok := conn.(*tls.Conn); ok {
				if p.defaultSessionAge > 0 {
					c.SetReadDeadline(coarsetime.CeilingTimeNow().Add(p.defaultSessionAge))
				}
				if p.defaultContextAge > 0 {
					c.SetReadDeadline(coarsetime.CeilingTimeNow().Add(p.defaultContextAge))
				}
				if err := c.Handshake(); err != nil {
					Errorf("TLS handshake error from %s: %s", c.RemoteAddr(), err.Error())
					return
				}
			}
			var sess = newSession(p, conn, protoFunc)
			if rerr := p.pluginContainer.PostAccept(sess); rerr != nil {
				sess.Close()
				return
			}
			Tracef("accept session(network:%s, addr:%s, id:%s)", network, sess.RemoteIp(), sess.Id())
			p.sessHub.Set(sess)
			sess.startReadAndHandle()
		})
	}
}

// ServeConn serves the connection and returns a session.
func (p *peer) ServeConn(conn net.Conn, protoFunc ...socket.ProtoFunc) (Session, error) {
	network := conn.LocalAddr().Network()
	if strings.Contains(network, "udp") {
		return nil, fmt.Errorf("invalid network: %s,\nrefer to the following: tcp, tcp4, tcp6, unix or unixpacket", network)
	}
	if c, ok := conn.(*tls.Conn); ok {
		if p.defaultSessionAge > 0 {
			c.SetReadDeadline(coarsetime.CeilingTimeNow().Add(p.defaultSessionAge))
		}
		if p.defaultContextAge > 0 {
			c.SetReadDeadline(coarsetime.CeilingTimeNow().Add(p.defaultContextAge))
		}
		if err := c.Handshake(); err != nil {
			return nil, errors.Errorf("TLS handshake error from %s: %s", c.RemoteAddr(), err.Error())
		}
	}
	var sess = newSession(p, conn, protoFunc)
	if rerr := p.pluginContainer.PostAccept(sess); rerr != nil {
		sess.Close()
		return nil, rerr.ToError()
	}
	Tracef("accept session(network:%s, addr:%s, id:%s)", network, sess.RemoteIp(), sess.Id())
	p.sessHub.Set(sess)
	AnywayGo(sess.startReadAndHandle)
	return sess, nil
}

// Close closes peer.
func (p *peer) Close() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = errors.Errorf("panic:\n%v\n%s", p, goutil.PanicTrace(2))
		}
	}()
	close(p.closeCh)
	deletePeer(p)
	var (
		count int
		errCh = make(chan error, 10)
	)
	p.sessHub.Range(func(sess *session) bool {
		count++
		if !Go(func() {
			errCh <- sess.Close()
		}) {
			errCh <- sess.Close()
		}
		return true
	})
	for i := 0; i < count; i++ {
		err = errors.Merge(err, <-errCh)
	}
	close(errCh)
	return err
}

func (p *peer) getContext(s *session, withWg bool) *readHandleCtx {
	p.ctxLock.Lock()
	if withWg {
		// count get context
		s.graceCtxWaitGroup.Add(1)
	}
	ctx := p.freeContext
	if ctx == nil {
		ctx = newReadHandleCtx()
	} else {
		p.freeContext = ctx.next
	}
	ctx.reInit(s)
	p.ctxLock.Unlock()
	return ctx
}

func (p *peer) putContext(ctx *readHandleCtx, withWg bool) {
	p.ctxLock.Lock()
	defer p.ctxLock.Unlock()
	if withWg {
		// count get context
		ctx.sess.graceCtxWaitGroup.Done()
	}
	ctx.clean()
	ctx.next = p.freeContext
	p.freeContext = ctx
}

// Router returns the root router of pull or push handlers.
func (p *peer) Router() *Router {
	return p.router
}

// SubRoute adds handler group.
func (p *peer) SubRoute(pathPrefix string, plugin ...Plugin) *SubRouter {
	return p.router.SubRoute(pathPrefix, plugin...)
}

// RoutePull registers PULL handlers, and returns the paths.
func (p *peer) RoutePull(pullCtrlStruct interface{}, plugin ...Plugin) []string {
	return p.router.RoutePull(pullCtrlStruct, plugin...)
}

// RoutePullFunc registers PULL handler, and returns the path.
func (p *peer) RoutePullFunc(pullHandleFunc interface{}, plugin ...Plugin) string {
	return p.router.RoutePullFunc(pullHandleFunc, plugin...)
}

// RoutePush registers PUSH handlers, and returns the paths.
func (p *peer) RoutePush(pushCtrlStruct interface{}, plugin ...Plugin) []string {
	return p.router.RoutePush(pushCtrlStruct, plugin...)
}

// RoutePushFunc registers PUSH handler, and returns the path.
func (p *peer) RoutePushFunc(pushHandleFunc interface{}, plugin ...Plugin) string {
	return p.router.RoutePushFunc(pushHandleFunc, plugin...)
}

// SetUnknownPull sets the default handler,
// which is called when no handler for PULL is found.
func (p *peer) SetUnknownPull(fn func(UnknownPullCtx) (interface{}, *Rerror), plugin ...Plugin) {
	p.router.SetUnknownPull(fn, plugin...)
}

// SetUnknownPush sets the default handler,
// which is called when no handler for PUSH is found.
func (p *peer) SetUnknownPush(fn func(UnknownPushCtx) *Rerror, plugin ...Plugin) {
	p.router.SetUnknownPush(fn, plugin...)
}

// maybe useful

func (p *peer) getPullHandler(uriPath string) (*Handler, bool) {
	return p.router.subRouter.getPull(uriPath)
}

func (p *peer) getPushHandler(uriPath string) (*Handler, bool) {
	return p.router.subRouter.getPush(uriPath)
}
