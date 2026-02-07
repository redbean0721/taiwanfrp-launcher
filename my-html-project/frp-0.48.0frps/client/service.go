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

package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/golib/crypto"
	libdial "github.com/fatedier/golib/net/dial"
	fmux "github.com/hashicorp/yamux"
	quic "github.com/quic-go/quic-go"

	"github.com/fatedier/frp/assets"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/transport"
	"github.com/fatedier/frp/pkg/util/log"
	frpNet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/xlog"
)

func init() {
	crypto.DefaultSalt = "frp"
	// TODO: remove this when we drop support for go1.19
	rand.Seed(time.Now().UnixNano())
}

// Service 是一個客戶端服務。
type Service struct {
	// 從 frps 獲取的唯一 ID，附加在 loginMsg 中
	runID string

	// 管理與伺服器的控制連接
	ctl   *Control
	ctlMu sync.RWMutex

	// 根據選擇的方法設置身份驗證
	authSetter auth.Setter

	cfg         config.ClientCommonConf
	pxyCfgs     map[string]config.ProxyConf
	visitorCfgs map[string]config.VisitorConf
	cfgMu       sync.RWMutex

	// 用於初始化此客戶端的配置文件，如果未使用配置文件則為空字符串
	cfgFile string

	// 這是由 frps 的登錄響應配置的
	serverUDPPort int

	exit uint32 // 0 表示未退出

	// 服務上下文
	ctx context.Context
	// 調用 cancel 以停止服務
	cancel context.CancelFunc
}

// NewService 創建一個新的 Service 實例
func NewService(
	cfg config.ClientCommonConf,
	pxyCfgs map[string]config.ProxyConf,
	visitorCfgs map[string]config.VisitorConf,
	cfgFile string,
) (svr *Service, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	svr = &Service{
		authSetter:  auth.NewAuthSetter(cfg.ClientConfig),
		cfg:         cfg,
		cfgFile:     cfgFile,
		pxyCfgs:     pxyCfgs,
		visitorCfgs: visitorCfgs,
		exit:        0,
		ctx:         xlog.NewContext(ctx, xlog.New()),
		cancel:      cancel,
	}
	return
}

// GetController 返回當前的控制器
func (svr *Service) GetController() *Control {
	svr.ctlMu.RLock()
	defer svr.ctlMu.RUnlock()
	return svr.ctl
}

// Run 啟動服務
func (svr *Service) Run() error {
	xl := xlog.FromContextSafe(svr.ctx)

	// 設置自定義的 DNSServer
	if svr.cfg.DNSServer != "" {
		dnsAddr := svr.cfg.DNSServer
		if _, _, err := net.SplitHostPort(dnsAddr); err != nil {
			dnsAddr = net.JoinHostPort(dnsAddr, "53")
		}
		 // 更改 frpc 的默認 DNS 伺服器
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return net.Dial("udp", dnsAddr)
			},
		}
	}

	// 登錄到 frps
	for {
		conn, cm, err := svr.login()
		if err != nil {
			xl.Warn("登錄到伺服器失敗: %v", err)

			// 如果 login_fail_exit 為 true，則直接退出此程序
			// 否則，稍等片刻再嘗試重新連接伺服器
			if svr.cfg.LoginFailExit {
				return err
			}
			util.RandomSleep(10*time.Second, 0.9, 1.1)
		} else {
			// 登錄成功
			ctl := NewControl(svr.ctx, svr.runID, conn, cm, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.serverUDPPort, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}

	go svr.keepControllerWorking()

	if svr.cfg.AdminPort != 0 {
		// 初始化管理伺服器資產
		assets.Load(svr.cfg.AssetsDir)

		address := net.JoinHostPort(svr.cfg.AdminAddr, strconv.Itoa(svr.cfg.AdminPort))
		err := svr.RunAdminServer(address)
		if err != nil {
			log.Warn("運行管理伺服器錯誤: %v", err)
		}
		log.Info("管理伺服器監聽於 %s:%d", svr.cfg.AdminAddr, svr.cfg.AdminPort)
	}
	<-svr.ctx.Done()
	return nil
}

// keepControllerWorking 保持控制器工作
func (svr *Service) keepControllerWorking() {
	xl := xlog.FromContextSafe(svr.ctx)
	maxDelayTime := 20 * time.Second
	delayTime := time.Second

	// 如果 frpc 重新連接 frps，我們需要在 1 分鐘內限制重試次數
	// 當前的重試邏輯是睡眠 0s, 0s, 0s, 1s, 2s, 4s, 8s, ...
	// 當超過 1 分鐘時，我們將重置延遲和計數
	cutoffTime := time.Now().Add(time.Minute)
	reconnectDelay := time.Second
	reconnectCounts := 1

	for {
		<-svr.ctl.ClosedDoneCh()
		if atomic.LoadUint32(&svr.exit) != 0 {
			return
		}

		// 前三次重試無延遲
		if reconnectCounts > 3 {
			util.RandomSleep(reconnectDelay, 0.9, 1.1)
			xl.Info("等待 %v 重新連接", reconnectDelay)
			reconnectDelay *= 2
		} else {
			util.RandomSleep(time.Second, 0, 0.5)
		}
		reconnectCounts++

		now := time.Now()
		if now.After(cutoffTime) {
			// 重置
			cutoffTime = now.Add(time.Minute)
			reconnectDelay = time.Second
			reconnectCounts = 1
		}

		for {
			if atomic.LoadUint32(&svr.exit) != 0 {
				return
			}

			xl.Info("嘗試重新連接到伺服器...")
			conn, cm, err := svr.login()
			if err != nil {
				xl.Warn("重新連接到伺服器錯誤: %v, 等待 %v 再次重試", err, delayTime)
				util.RandomSleep(delayTime, 0.9, 1.1)

				delayTime *= 2
				if delayTime > maxDelayTime {
					delayTime = maxDelayTime
				}
				continue
			}
			// 重新連接成功，初始化 delayTime
			delayTime = time.Second

			ctl := NewControl(svr.ctx, svr.runID, conn, cm, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.serverUDPPort, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			if svr.ctl != nil {
				svr.ctl.Close()
			}
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}
}

// login 創建到 frps 的連接並註冊自己為客戶端
// conn: 控制連接
// session: 如果不為 nil，則使用 tcp mux
func (svr *Service) login() (conn net.Conn, cm *ConnectionManager, err error) {
	xl := xlog.FromContextSafe(svr.ctx)
	cm = NewConnectionManager(svr.ctx, &svr.cfg)

	if err = cm.OpenConnection(); err != nil {
		return nil, nil, err
	}

	defer func() {
		if err != nil {
			cm.Close()
		}
	}()

	conn, err = cm.Connect()
	if err != nil {
		return
	}

	loginMsg := &msg.Login{
		Arch:      runtime.GOARCH,
		Os:        runtime.GOOS,
		PoolCount: svr.cfg.PoolCount,
		User:      svr.cfg.User,
		Version:   version.Full(),
		Timestamp: time.Now().Unix(),
		RunID:     svr.runID,
		Metas:     svr.cfg.Metas,
	}

	// 添加身份驗證
	if err = svr.authSetter.SetLogin(loginMsg); err != nil {
		return
	}

	if err = msg.WriteMsg(conn, loginMsg); err != nil {
		return
	}

	var loginRespMsg msg.LoginResp
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = msg.ReadMsgInto(conn, &loginRespMsg); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if loginRespMsg.Error != "" {
		err = fmt.Errorf("%s", loginRespMsg.Error)
		xl.Error("%s", loginRespMsg.Error)
		return
	}

	svr.runID = loginRespMsg.RunID
	xl.ResetPrefixes()
	xl.AppendPrefix(svr.runID)

	svr.serverUDPPort = loginRespMsg.ServerUDPPort
	xl.Info("登錄到伺服器成功，獲取運行 ID [%s]，伺服器 UDP 埠 [%d]", loginRespMsg.RunID, loginRespMsg.ServerUDPPort)
	return
}

// ReloadConf 重新加載配置
func (svr *Service) ReloadConf(pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf) error {
	svr.cfgMu.Lock()
	svr.pxyCfgs = pxyCfgs
	svr.visitorCfgs = visitorCfgs
	svr.cfgMu.Unlock()

	svr.ctlMu.RLock()
	ctl := svr.ctl
	svr.ctlMu.RUnlock()

	if ctl != nil {
		return svr.ctl.ReloadConf(pxyCfgs, visitorCfgs)
	}
	return nil
}

// Close 關閉服務
func (svr *Service) Close() {
	svr.GracefulClose(time.Duration(0))
}

// GracefulClose 優雅地關閉服務
func (svr *Service) GracefulClose(d time.Duration) {
	atomic.StoreUint32(&svr.exit, 1)

	svr.ctlMu.RLock()
	if svr.ctl != nil {
		svr.ctl.GracefulClose(d)
	}
	svr.ctlMu.RUnlock()

	svr.cancel()
}

// ConnectionManager 管理連接
type ConnectionManager struct {
	ctx context.Context
	cfg *config.ClientCommonConf

	muxSession *fmux.Session
	quicConn   quic.Connection
}

// NewConnectionManager 創建一個新的 ConnectionManager 實例
func NewConnectionManager(ctx context.Context, cfg *config.ClientCommonConf) *ConnectionManager {
	return &ConnectionManager{
		ctx: ctx,
		cfg: cfg,
	}
}

// OpenConnection 打開連接
func (cm *ConnectionManager) OpenConnection() error {
	xl := xlog.FromContextSafe(cm.ctx)

	// 特殊處理 quic
	if strings.EqualFold(cm.cfg.Protocol, "quic") {
		var tlsConfig *tls.Config
		var err error
		sn := cm.cfg.TLSServerName
		if sn == "" {
			sn = cm.cfg.ServerAddr
		}
		if cm.cfg.TLSEnable {
			tlsConfig, err = transport.NewClientTLSConfig(
				cm.cfg.TLSCertFile,
				cm.cfg.TLSKeyFile,
				cm.cfg.TLSTrustedCaFile,
				sn)
		} else {
			tlsConfig, err = transport.NewClientTLSConfig("", "", "", sn)
		}
		if err != nil {
			xl.Warn("構建 tls 配置失敗，錯誤: %v", err)
			return err
		}
		tlsConfig.NextProtos = []string{"frp"}

		conn, err := quic.DialAddr(
			net.JoinHostPort(cm.cfg.ServerAddr, strconv.Itoa(cm.cfg.ServerPort)),
			tlsConfig, &quic.Config{
				MaxIdleTimeout:     time.Duration(cm.cfg.QUICMaxIdleTimeout) * time.Second,
				MaxIncomingStreams: int64(cm.cfg.QUICMaxIncomingStreams),
				KeepAlivePeriod:    time.Duration(cm.cfg.QUICKeepalivePeriod) * time.Second,
			})
		if err != nil {
			return err
		}
		cm.quicConn = conn
		return nil
	}

	if !cm.cfg.TCPMux {
		return nil
	}

	conn, err := cm.realConnect()
	if err != nil {
		return err
	}

	fmuxCfg := fmux.DefaultConfig()
	fmuxCfg.KeepAliveInterval = time.Duration(cm.cfg.TCPMuxKeepaliveInterval) * time.Second
	fmuxCfg.LogOutput = io.Discard
	session, err := fmux.Client(conn, fmuxCfg)
	if err != nil {
		return err
	}
	cm.muxSession = session
	return nil
}

// Connect 連接到伺服器
func (cm *ConnectionManager) Connect() (net.Conn, error) {
	if cm.quicConn != nil {
		stream, err := cm.quicConn.OpenStreamSync(context.Background())
		if err != nil {
			return nil, err
		}
		return frpNet.QuicStreamToNetConn(stream, cm.quicConn), nil
	} else if cm.muxSession != nil {
		stream, err := cm.muxSession.OpenStream()
		if err != nil {
			return nil, err
		}
		return stream, nil
	}

	return cm.realConnect()
}

// realConnect 實際連接到伺服器
func (cm *ConnectionManager) realConnect() (net.Conn, error) {
	xl := xlog.FromContextSafe(cm.ctx)
	var tlsConfig *tls.Config
	var err error
	if cm.cfg.TLSEnable {
		sn := cm.cfg.TLSServerName
		if sn == "" {
			sn = cm.cfg.ServerAddr
		}

		tlsConfig, err = transport.NewClientTLSConfig(
			cm.cfg.TLSCertFile,
			cm.cfg.TLSKeyFile,
			cm.cfg.TLSTrustedCaFile,
			sn)
		if err != nil {
			xl.Warn("構建 tls 配置失敗，錯誤: %v", err)
			return nil, err
		}
	}

	proxyType, addr, auth, err := libdial.ParseProxyURL(cm.cfg.HTTPProxy)
	if err != nil {
		xl.Error("解析代理 url 失敗")
		return nil, err
	}
	dialOptions := []libdial.DialOption{}
	protocol := cm.cfg.Protocol
	if protocol == "websocket" {
		protocol = "tcp"
		dialOptions = append(dialOptions, libdial.WithAfterHook(libdial.AfterHook{Hook: frpNet.DialHookWebsocket()}))
	}
	if cm.cfg.ConnectServerLocalIP != "" {
		dialOptions = append(dialOptions, libdial.WithLocalAddr(cm.cfg.ConnectServerLocalIP))
	}
	dialOptions = append(dialOptions,
		libdial.WithProtocol(protocol),
		libdial.WithTimeout(time.Duration(cm.cfg.DialServerTimeout)*time.Second),
		libdial.WithKeepAlive(time.Duration(cm.cfg.DialServerKeepAlive)*time.Second),
		libdial.WithProxy(proxyType, addr),
		libdial.WithProxyAuth(auth),
		libdial.WithTLSConfig(tlsConfig),
		libdial.WithAfterHook(libdial.AfterHook{
			Hook: frpNet.DialHookCustomTLSHeadByte(tlsConfig != nil, cm.cfg.DisableCustomTLSFirstByte),
		}),
	)
	conn, err := libdial.Dial(
		net.JoinHostPort(cm.cfg.ServerAddr, strconv.Itoa(cm.cfg.ServerPort)),
		dialOptions...,
	)
	return conn, err
}

// Close 關閉連接
func (cm *ConnectionManager) Close() error {
	if cm.quicConn != nil {
		_ = cm.quicConn.CloseWithError(0, "")
	}
	if cm.muxSession != nil {
		_ = cm.muxSession.Close()
	}
	return nil
}
