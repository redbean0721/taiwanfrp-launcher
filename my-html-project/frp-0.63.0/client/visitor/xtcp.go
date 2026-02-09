// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package visitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	libio "github.com/fatedier/golib/io"
	fmux "github.com/hashicorp/yamux"
	quic "github.com/quic-go/quic-go"
	"golang.org/x/time/rate"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/nathole"
	"github.com/fatedier/frp/pkg/transport"
	netpkg "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/xlog"
)

var ErrNoTunnelSession = errors.New("no tunnel session")

type XTCPVisitor struct {
	*BaseVisitor
	session       TunnelSession
	startTunnelCh chan struct{}
	retryLimiter  *rate.Limiter
	cancel        context.CancelFunc

	cfg *v1.XTCPVisitorConfig
}

func (sv *XTCPVisitor) Run() (err error) {
	sv.ctx, sv.cancel = context.WithCancel(sv.ctx)

	if sv.cfg.Protocol == "kcp" {
		sv.session = NewKCPTunnelSession()
	} else {
		sv.session = NewQUICTunnelSession(sv.clientCfg)
	}

	if sv.cfg.BindPort > 0 {
		sv.l, err = net.Listen("tcp", net.JoinHostPort(sv.cfg.BindAddr, strconv.Itoa(sv.cfg.BindPort)))
		if err != nil {
			return
		}
		go sv.worker()
	}

	go sv.internalConnWorker()
	go sv.processTunnelStartEvents()
	if sv.cfg.KeepTunnelOpen {
		sv.retryLimiter = rate.NewLimiter(rate.Every(time.Hour/time.Duration(sv.cfg.MaxRetriesAnHour)), sv.cfg.MaxRetriesAnHour)
		go sv.keepTunnelOpenWorker()
	}

	if sv.plugin != nil {
		sv.plugin.Start()
	}
	return
}

func (sv *XTCPVisitor) Close() {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.BaseVisitor.Close()
	if sv.cancel != nil {
		sv.cancel()
	}
	if sv.session != nil {
		sv.session.Close()
	}
}

func (sv *XTCPVisitor) worker() {
	xl := xlog.FromContextSafe(sv.ctx)
	for {
		conn, err := sv.l.Accept()
		if err != nil {
			xl.Warnf("xtcp 本地監聽器已關閉")
			return
		}
		go sv.handleConn(conn)
	}
}

func (sv *XTCPVisitor) internalConnWorker() {
	xl := xlog.FromContextSafe(sv.ctx)
	for {
		conn, err := sv.internalLn.Accept()
		if err != nil {
			xl.Warnf("xtcp 內部監聽器已關閉")
			return
		}
		go sv.handleConn(conn)
	}
}

func (sv *XTCPVisitor) processTunnelStartEvents() {
	for {
		select {
		case <-sv.ctx.Done():
			return
		case <-sv.startTunnelCh:
			start := time.Now()
			sv.makeNatHole()
			duration := time.Since(start)
			// avoid too frequently
			if duration < 10*time.Second {
				time.Sleep(10*time.Second - duration)
			}
		}
	}
}

func (sv *XTCPVisitor) keepTunnelOpenWorker() {
	xl := xlog.FromContextSafe(sv.ctx)
	ticker := time.NewTicker(time.Duration(sv.cfg.MinRetryInterval) * time.Second)
	defer ticker.Stop()

	sv.startTunnelCh <- struct{}{}
	for {
		select {
		case <-sv.ctx.Done():
			return
		case <-ticker.C:
			xl.Debugf("keepTunnelOpenWorker 嘗試檢查通道...")
			conn, err := sv.getTunnelConn()
			if err != nil {
				xl.Warnf("keepTunnelOpenWorker 取得通道連線失敗: %v", err)
				_ = sv.retryLimiter.Wait(sv.ctx)
				continue
			}
			xl.Debugf("keepTunnelOpenWorker 檢查成功")
			if conn != nil {
				conn.Close()
			}
		}
	}
}

func (sv *XTCPVisitor) handleConn(userConn net.Conn) {
	xl := xlog.FromContextSafe(sv.ctx)
	isConnTransfered := false
	defer func() {
		if !isConnTransfered {
			userConn.Close()
		}
	}()

	xl.Debugf("取得一個新的 xtcp 使用者連線")

	// Open a tunnel connection to the server. If there is already a successful hole-punching connection,
	// it will be reused. Otherwise, it will block and wait for a successful hole-punching connection until timeout.
	ctx := context.Background()
	if sv.cfg.FallbackTo != "" {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(sv.cfg.FallbackTimeoutMs)*time.Millisecond)
		defer cancel()
		ctx = timeoutCtx
	}
	tunnelConn, err := sv.openTunnel(ctx)
	if err != nil {
		xl.Errorf("開啟通道失敗: %v", err)
		// no fallback, just return
		if sv.cfg.FallbackTo == "" {
			return
		}

		xl.Debugf("嘗試轉移連線給訪客: %s", sv.cfg.FallbackTo)
		if err := sv.helper.TransferConn(sv.cfg.FallbackTo, userConn); err != nil {
			xl.Errorf("轉移連線給訪客 %s 失敗: %v", sv.cfg.FallbackTo, err)
			return
		}
		isConnTransfered = true
		return
	}

	var muxConnRWCloser io.ReadWriteCloser = tunnelConn
	if sv.cfg.Transport.UseEncryption {
		muxConnRWCloser, err = libio.WithEncryption(muxConnRWCloser, []byte(sv.cfg.SecretKey))
		if err != nil {
			xl.Errorf("建立加密串流失敗: %v", err)
			return
		}
	}
	if sv.cfg.Transport.UseCompression {
		var recycleFn func()
		muxConnRWCloser, recycleFn = libio.WithCompressionFromPool(muxConnRWCloser)
		defer recycleFn()
	}

	_, _, errs := libio.Join(userConn, muxConnRWCloser)
	xl.Debugf("連線已關閉")
	if len(errs) > 0 {
		xl.Tracef("連線關閉時發生錯誤: %v", errs)
	}
}

// openTunnel will open a tunnel connection to the target server.
func (sv *XTCPVisitor) openTunnel(ctx context.Context) (conn net.Conn, err error) {
	xl := xlog.FromContextSafe(sv.ctx)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	timeoutC := time.After(20 * time.Second)
	immediateTrigger := make(chan struct{}, 1)
	defer close(immediateTrigger)
	immediateTrigger <- struct{}{}

	for {
		select {
		case <-sv.ctx.Done():
			return nil, sv.ctx.Err()
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-immediateTrigger:
			conn, err = sv.getTunnelConn()
		case <-ticker.C:
			conn, err = sv.getTunnelConn()
		case <-timeoutC:
			return nil, fmt.Errorf("開啟通道超時")
		}

		if err != nil {
			if err != ErrNoTunnelSession {
				xl.Warnf("取得通道連線失敗: %v", err)
			}
			continue
		}
		return conn, nil
	}
}

func (sv *XTCPVisitor) getTunnelConn() (net.Conn, error) {
	conn, err := sv.session.OpenConn(sv.ctx)
	if err == nil {
		return conn, nil
	}
	sv.session.Close()

	select {
	case sv.startTunnelCh <- struct{}{}:
	default:
	}
	return nil, err
}

// 0. PreCheck
// 1. Prepare
// 2. ExchangeInfo
// 3. MakeNATHole
// 4. Create a tunnel session using an underlying UDP connection.
func (sv *XTCPVisitor) makeNatHole() {
	xl := xlog.FromContextSafe(sv.ctx)
	xl.Tracef("開始進行 NAT 打洞")
	if err := nathole.PreCheck(sv.ctx, sv.helper.MsgTransporter(), sv.cfg.ServerName, 5*time.Second); err != nil {
		xl.Warnf("nathole 預檢查失敗: %v", err)
		return
	}

	xl.Tracef("nathole 準備階段開始")
	prepareResult, err := nathole.Prepare([]string{sv.clientCfg.NatHoleSTUNServer})
	if err != nil {
		xl.Warnf("nathole 準備階段失敗: %v", err)
		return
	}
	xl.Infof("nathole 準備成功，NAT 類型: %s，行為: %s，位址: %v，協助位址: %v",
		prepareResult.NatType, prepareResult.Behavior, prepareResult.Addrs, prepareResult.AssistedAddrs)

	listenConn := prepareResult.ListenConn

	// send NatHoleVisitor to server
	now := time.Now().Unix()
	transactionID := nathole.NewTransactionID()
	natHoleVisitorMsg := &msg.NatHoleVisitor{
		TransactionID: transactionID,
		ProxyName:     sv.cfg.ServerName,
		Protocol:      sv.cfg.Protocol,
		SignKey:       util.GetAuthKey(sv.cfg.SecretKey, now),
		Timestamp:     now,
		MappedAddrs:   prepareResult.Addrs,
		AssistedAddrs: prepareResult.AssistedAddrs,
	}

	xl.Tracef("nathole 資訊交換階段開始")
	natHoleRespMsg, err := nathole.ExchangeInfo(sv.ctx, sv.helper.MsgTransporter(), transactionID, natHoleVisitorMsg, 5*time.Second)
	if err != nil {
		listenConn.Close()
		xl.Warnf("nathole 資訊交換失敗: %v", err)
		return
	}

	xl.Infof("取得 natHoleRespMsg，sid [%s]，協定 [%s]，候選位址 %v，協助位址 %v，偵測行為: %+v",
		natHoleRespMsg.Sid, natHoleRespMsg.Protocol, natHoleRespMsg.CandidateAddrs,
		natHoleRespMsg.AssistedAddrs, natHoleRespMsg.DetectBehavior)

	newListenConn, raddr, err := nathole.MakeHole(sv.ctx, listenConn, natHoleRespMsg, []byte(sv.cfg.SecretKey))
	if err != nil {
		listenConn.Close()
		xl.Warnf("打洞失敗: %v", err)
		return
	}
	listenConn = newListenConn
	xl.Infof("NAT 打洞連線建立成功，sid [%s]，遠端位址 [%s]", natHoleRespMsg.Sid, raddr)

	if err := sv.session.Init(listenConn, raddr); err != nil {
		listenConn.Close()
		xl.Warnf("初始化通道 session 失敗: %v", err)
		return
	}
}

type TunnelSession interface {
	Init(listenConn *net.UDPConn, raddr *net.UDPAddr) error
	OpenConn(context.Context) (net.Conn, error)
	Close()
}

type KCPTunnelSession struct {
	session *fmux.Session
	lConn   *net.UDPConn
	mu      sync.RWMutex
}

func NewKCPTunnelSession() TunnelSession {
	return &KCPTunnelSession{}
}

func (ks *KCPTunnelSession) Init(listenConn *net.UDPConn, raddr *net.UDPAddr) error {
	listenConn.Close()
	laddr, _ := net.ResolveUDPAddr("udp", listenConn.LocalAddr().String())
	lConn, err := net.DialUDP("udp", laddr, raddr)
	if err != nil {
		return fmt.Errorf("dial udp error: %v", err)
	}
	remote, err := netpkg.NewKCPConnFromUDP(lConn, true, raddr.String())
	if err != nil {
		return fmt.Errorf("create kcp connection from udp connection error: %v", err)
	}

	fmuxCfg := fmux.DefaultConfig()
	fmuxCfg.KeepAliveInterval = 10 * time.Second
	fmuxCfg.MaxStreamWindowSize = 6 * 1024 * 1024
	fmuxCfg.LogOutput = io.Discard
	session, err := fmux.Client(remote, fmuxCfg)
	if err != nil {
		remote.Close()
		return fmt.Errorf("initial client session error: %v", err)
	}
	ks.mu.Lock()
	ks.session = session
	ks.lConn = lConn
	ks.mu.Unlock()
	return nil
}

func (ks *KCPTunnelSession) OpenConn(_ context.Context) (net.Conn, error) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	session := ks.session
	if session == nil {
		return nil, ErrNoTunnelSession
	}
	return session.Open()
}

func (ks *KCPTunnelSession) Close() {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.session != nil {
		_ = ks.session.Close()
		ks.session = nil
	}
	if ks.lConn != nil {
		_ = ks.lConn.Close()
		ks.lConn = nil
	}
}

type QUICTunnelSession struct {
	session    quic.Connection
	listenConn *net.UDPConn
	mu         sync.RWMutex

	clientCfg *v1.ClientCommonConfig
}

func NewQUICTunnelSession(clientCfg *v1.ClientCommonConfig) TunnelSession {
	return &QUICTunnelSession{
		clientCfg: clientCfg,
	}
}

func (qs *QUICTunnelSession) Init(listenConn *net.UDPConn, raddr *net.UDPAddr) error {
	tlsConfig, err := transport.NewClientTLSConfig("", "", "", raddr.String())
	if err != nil {
		return fmt.Errorf("create tls config error: %v", err)
	}
	tlsConfig.NextProtos = []string{"frp"}
	quicConn, err := quic.Dial(context.Background(), listenConn, raddr, tlsConfig,
		&quic.Config{
			MaxIdleTimeout:     time.Duration(qs.clientCfg.Transport.QUIC.MaxIdleTimeout) * time.Second,
			MaxIncomingStreams: int64(qs.clientCfg.Transport.QUIC.MaxIncomingStreams),
			KeepAlivePeriod:    time.Duration(qs.clientCfg.Transport.QUIC.KeepalivePeriod) * time.Second,
		})
	if err != nil {
		return fmt.Errorf("dial quic error: %v", err)
	}
	qs.mu.Lock()
	qs.session = quicConn
	qs.listenConn = listenConn
	qs.mu.Unlock()
	return nil
}

func (qs *QUICTunnelSession) OpenConn(ctx context.Context) (net.Conn, error) {
	qs.mu.RLock()
	defer qs.mu.RUnlock()
	session := qs.session
	if session == nil {
		return nil, ErrNoTunnelSession
	}
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return netpkg.QuicStreamToNetConn(stream, session), nil
}

func (qs *QUICTunnelSession) Close() {
	qs.mu.Lock()
	defer qs.mu.Unlock()
	if qs.session != nil {
		_ = qs.session.CloseWithError(0, "")
		qs.session = nil
	}
	if qs.listenConn != nil {
		_ = qs.listenConn.Close()
		qs.listenConn = nil
	}
}
