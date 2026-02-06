// 版權所有 2017 fatedier, fatedier@gmail.com
//
// 根據 Apache 許可證 2.0 版（「許可證」）授權；
// 除非遵守許可證，否則您不得使用此文件。
// 您可以在以下位置獲得許可證副本
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// 除非適用法律要求或書面同意，否則根據許可證分發的軟體
// 是按「原樣」分發的，不附帶任何明示或暗示的保證或條件。
// 有關許可證下特定語言的權限和限制，請參閱許可證。

package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatedier/golib/errors"
	frpIo "github.com/fatedier/golib/io"
	libdial "github.com/fatedier/golib/net/dial"
	"github.com/fatedier/golib/pool"
	fmux "github.com/hashicorp/yamux"
	pp "github.com/pires/go-proxyproto"
	"golang.org/x/time/rate"

	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	plugin "github.com/fatedier/frp/pkg/plugin/client"
	"github.com/fatedier/frp/pkg/proto/udp"
	"github.com/fatedier/frp/pkg/util/limit"
	frpNet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/xlog"
)

// 代理介面定義不同代理類型的工作連線處理方式。
type Proxy interface {
	Run() error

	// 接收在伺服器上註冊的工作連線。
	InWorkConn(net.Conn, *msg.StartWorkConn)

	Close()
}

func NewProxy(ctx context.Context, pxyConf config.ProxyConf, clientCfg config.ClientCommonConf, serverUDPPort int) (pxy Proxy) {
	var limiter *rate.Limiter
	limitBytes := pxyConf.GetBaseInfo().BandwidthLimit.Bytes()
	if limitBytes > 0 && pxyConf.GetBaseInfo().BandwidthLimitMode == config.BandwidthLimitModeClient {
		limiter = rate.NewLimiter(rate.Limit(float64(limitBytes)), int(limitBytes))
	}

	baseProxy := BaseProxy{
		clientCfg:     clientCfg,
		serverUDPPort: serverUDPPort,
		limiter:       limiter,
		xl:            xlog.FromContextSafe(ctx),
		ctx:           ctx,
	}
	switch cfg := pxyConf.(type) {
	case *config.TCPProxyConf:
		pxy = &TCPProxy{
			BaseProxy: &baseProxy,
			cfg:       cfg,
		}
	case *config.TCPMuxProxyConf:
		pxy = &TCPMuxProxy{
			BaseProxy: &baseProxy,
			cfg:       cfg,
		}
	case *config.UDPProxyConf:
		pxy = &UDPProxy{
			BaseProxy: &baseProxy,
			cfg:       cfg,
		}
	case *config.HTTPProxyConf:
		pxy = &HTTPProxy{
			BaseProxy: &baseProxy,
			cfg:       cfg,
		}
	case *config.HTTPSProxyConf:
		pxy = &HTTPSProxy{
			BaseProxy: &baseProxy,
			cfg:       cfg,
		}
	case *config.STCPProxyConf:
		pxy = &STCPProxy{
			BaseProxy: &baseProxy,
			cfg:       cfg,
		}
	case *config.XTCPProxyConf:
		pxy = &XTCPProxy{
			BaseProxy: &baseProxy,
			cfg:       cfg,
		}
	case *config.SUDPProxyConf:
		pxy = &SUDPProxy{
			BaseProxy: &baseProxy,
			cfg:       cfg,
			closeCh:   make(chan struct{}),
		}
	}
	return
}

type BaseProxy struct {
	closed        bool
	clientCfg     config.ClientCommonConf
	serverUDPPort int
	limiter       *rate.Limiter

	mu  sync.RWMutex
	xl  *xlog.Logger
	ctx context.Context
}

// TCP 代理
type TCPProxy struct {
	*BaseProxy

	cfg         *config.TCPProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *TCPProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *TCPProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *TCPProxy) InWorkConn(conn net.Conn, m *msg.StartWorkConn) {
	HandleTCPWorkConnection(pxy.ctx, &pxy.cfg.LocalSvrConf, pxy.proxyPlugin, pxy.cfg.GetBaseInfo(), pxy.limiter,
		conn, []byte(pxy.clientCfg.Token), m)
}

// TCP 多工代理
type TCPMuxProxy struct {
	*BaseProxy

	cfg         *config.TCPMuxProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *TCPMuxProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *TCPMuxProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *TCPMuxProxy) InWorkConn(conn net.Conn, m *msg.StartWorkConn) {
	HandleTCPWorkConnection(pxy.ctx, &pxy.cfg.LocalSvrConf, pxy.proxyPlugin, pxy.cfg.GetBaseInfo(), pxy.limiter,
		conn, []byte(pxy.clientCfg.Token), m)
}

// HTTP 代理
type HTTPProxy struct {
	*BaseProxy

	cfg         *config.HTTPProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *HTTPProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *HTTPProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *HTTPProxy) InWorkConn(conn net.Conn, m *msg.StartWorkConn) {
	HandleTCPWorkConnection(pxy.ctx, &pxy.cfg.LocalSvrConf, pxy.proxyPlugin, pxy.cfg.GetBaseInfo(), pxy.limiter,
		conn, []byte(pxy.clientCfg.Token), m)
}

// HTTPS 代理
type HTTPSProxy struct {
	*BaseProxy

	cfg         *config.HTTPSProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *HTTPSProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *HTTPSProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *HTTPSProxy) InWorkConn(conn net.Conn, m *msg.StartWorkConn) {
	HandleTCPWorkConnection(pxy.ctx, &pxy.cfg.LocalSvrConf, pxy.proxyPlugin, pxy.cfg.GetBaseInfo(), pxy.limiter,
		conn, []byte(pxy.clientCfg.Token), m)
}

// STCP 代理
type STCPProxy struct {
	*BaseProxy

	cfg         *config.STCPProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *STCPProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *STCPProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *STCPProxy) InWorkConn(conn net.Conn, m *msg.StartWorkConn) {
	HandleTCPWorkConnection(pxy.ctx, &pxy.cfg.LocalSvrConf, pxy.proxyPlugin, pxy.cfg.GetBaseInfo(), pxy.limiter,
		conn, []byte(pxy.clientCfg.Token), m)
}

// XTCP 代理
type XTCPProxy struct {
	*BaseProxy

	cfg         *config.XTCPProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *XTCPProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *XTCPProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *XTCPProxy) InWorkConn(conn net.Conn, m *msg.StartWorkConn) {
	xl := pxy.xl
	defer conn.Close()
	var natHoleSidMsg msg.NatHoleSid
	err := msg.ReadMsgInto(conn, &natHoleSidMsg)
	if err != nil {
		xl.Error("xtcp 從工作連線讀取失敗: %v", err)
		return
	}

	natHoleClientMsg := &msg.NatHoleClient{
		ProxyName: pxy.cfg.ProxyName,
		Sid:       natHoleSidMsg.Sid,
	}
	raddr, _ := net.ResolveUDPAddr("udp",
		net.JoinHostPort(pxy.clientCfg.ServerAddr, strconv.Itoa(pxy.serverUDPPort)))
	clientConn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		xl.Error("連線伺服器 UDP 位址失敗: %v", err)
		return
	}
	defer clientConn.Close()

	err = msg.WriteMsg(clientConn, natHoleClientMsg)
	if err != nil {
		xl.Error("送出 natHoleClientMsg 到伺服器失敗: %v", err)
		return
	}

	// 最多等待 5 秒取得客戶端位址。
	var natHoleRespMsg msg.NatHoleResp
	_ = clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))

	buf := pool.GetBuf(1024)
	n, err := clientConn.Read(buf)
	if err != nil {
		xl.Error("取得 natHoleRespMsg 失敗: %v", err)
		return
	}
	err = msg.ReadMsgInto(bytes.NewReader(buf[:n]), &natHoleRespMsg)
	if err != nil {
		xl.Error("取得 natHoleRespMsg 失敗: %v", err)
		return
	}
	_ = clientConn.SetReadDeadline(time.Time{})
	_ = clientConn.Close()

	if natHoleRespMsg.Error != "" {
		xl.Error("natHoleRespMsg 錯誤資訊: %s", natHoleRespMsg.Error)
		return
	}

	xl.Trace("取得 natHoleRespMsg，sid [%s]，客戶端位址 [%s]，訪客位址 [%s]", natHoleRespMsg.Sid, natHoleRespMsg.ClientAddr, natHoleRespMsg.VisitorAddr)

	// 送出偵測訊息
	host, portStr, err := net.SplitHostPort(natHoleRespMsg.VisitorAddr)
	if err != nil {
		xl.Error("取得 NatHoleResp 訪客位址 [%s] 失敗: %v", natHoleRespMsg.VisitorAddr, err)
	}
	laddr, _ := net.ResolveUDPAddr("udp", clientConn.LocalAddr().String())

	port, err := strconv.ParseInt(portStr, 10, 64)
	if err != nil {
		xl.Error("解析 natHoleResp 訪客位址失敗: %v", natHoleRespMsg.VisitorAddr)
		return
	}
	_ = pxy.sendDetectMsg(host, int(port), laddr, []byte(natHoleRespMsg.Sid))
	xl.Trace("已送出所有偵測訊息")

	if err := msg.WriteMsg(conn, &msg.NatHoleClientDetectOK{}); err != nil {
		xl.Error("寫入訊息失敗: %v", err)
		return
	}

	// 監聽客戶端連線位址並等待訪客連線
	lConn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		xl.Error("監聽訪客連線本地位址失敗: %v", err)
		return
	}
	defer lConn.Close()

	_ = lConn.SetReadDeadline(time.Now().Add(8 * time.Second))
	sidBuf := pool.GetBuf(1024)
	var uAddr *net.UDPAddr
	n, uAddr, err = lConn.ReadFromUDP(sidBuf)
	if err != nil {
		xl.Warn("從訪客取得 sid 失敗: %v", err)
		return
	}
	_ = lConn.SetReadDeadline(time.Time{})
	if string(sidBuf[:n]) != natHoleRespMsg.Sid {
		xl.Warn("訪客回傳的 sid 不正確")
		return
	}
	pool.PutBuf(sidBuf)
	xl.Info("NAT 打洞連線成功，sid [%s]", natHoleRespMsg.Sid)

	if _, err := lConn.WriteToUDP(sidBuf[:n], uAddr); err != nil {
		xl.Error("寫入 uaddr 失敗: %v", err)
		return
	}

	kcpConn, err := frpNet.NewKCPConnFromUDP(lConn, false, uAddr.String())
	if err != nil {
		xl.Error("從 UDP 連線建立 KCP 連線失敗: %v", err)
		return
	}

	fmuxCfg := fmux.DefaultConfig()
	fmuxCfg.KeepAliveInterval = 5 * time.Second
	fmuxCfg.LogOutput = io.Discard
	sess, err := fmux.Server(kcpConn, fmuxCfg)
	if err != nil {
		xl.Error("從 KCP 連線建立 yamux 伺服器失敗: %v", err)
		return
	}
	defer sess.Close()
	muxConn, err := sess.Accept()
	if err != nil {
		xl.Error("接受 yamux 連線失敗: %v", err)
		return
	}

	HandleTCPWorkConnection(pxy.ctx, &pxy.cfg.LocalSvrConf, pxy.proxyPlugin, pxy.cfg.GetBaseInfo(), pxy.limiter,
		muxConn, []byte(pxy.cfg.Sk), m)
}

func (pxy *XTCPProxy) sendDetectMsg(addr string, port int, laddr *net.UDPAddr, content []byte) (err error) {
	daddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(addr, strconv.Itoa(port)))
	if err != nil {
		return err
	}

	tConn, err := net.DialUDP("udp", laddr, daddr)
	if err != nil {
		return err
	}

	// 如需調整 TTL，可使用 ipv4.NewConn 包裝 tConn 再設定

	if _, err := tConn.Write(content); err != nil {
		return err
	}
	return tConn.Close()
}

// UDP 代理
type UDPProxy struct {
	*BaseProxy

	cfg *config.UDPProxyConf

	localAddr *net.UDPAddr
	readCh    chan *msg.UDPPacket

	// 包含 msg.UDPPacket 與 msg.Ping
	sendCh   chan msg.Message
	workConn net.Conn
}

func (pxy *UDPProxy) Run() (err error) {
	pxy.localAddr, err = net.ResolveUDPAddr("udp", net.JoinHostPort(pxy.cfg.LocalIP, strconv.Itoa(pxy.cfg.LocalPort)))
	if err != nil {
		return
	}
	return
}

func (pxy *UDPProxy) Close() {
	pxy.mu.Lock()
	defer pxy.mu.Unlock()

	if !pxy.closed {
		pxy.closed = true
		if pxy.workConn != nil {
			pxy.workConn.Close()
		}
		if pxy.readCh != nil {
			close(pxy.readCh)
		}
		if pxy.sendCh != nil {
			close(pxy.sendCh)
		}
	}
}

func (pxy *UDPProxy) InWorkConn(conn net.Conn, m *msg.StartWorkConn) {
	xl := pxy.xl
	xl.Info("收到一個新的 UDP 代理工作連線, %s", conn.RemoteAddr().String())
	// 關閉舊工作連線相關資源
	pxy.Close()

	var rwc io.ReadWriteCloser = conn
	var err error
	if pxy.limiter != nil {
		rwc = frpIo.WrapReadWriteCloser(limit.NewReader(conn, pxy.limiter), limit.NewWriter(conn, pxy.limiter), func() error {
			return conn.Close()
		})
	}
	if pxy.cfg.UseEncryption {
		rwc, err = frpIo.WithEncryption(rwc, []byte(pxy.clientCfg.Token))
		if err != nil {
			conn.Close()
			xl.Error("建立加密串流失敗: %v", err)
			return
		}
	}
	if pxy.cfg.UseCompression {
		rwc = frpIo.WithCompression(rwc)
	}
	conn = frpNet.WrapReadWriteCloserToConn(rwc, conn)

	pxy.mu.Lock()
	pxy.workConn = conn
	pxy.readCh = make(chan *msg.UDPPacket, 1024)
	pxy.sendCh = make(chan msg.Message, 1024)
	pxy.closed = false
	pxy.mu.Unlock()

	workConnReaderFn := func(conn net.Conn, readCh chan *msg.UDPPacket) {
		for {
			var udpMsg msg.UDPPacket
			if errRet := msg.ReadMsgInto(conn, &udpMsg); errRet != nil {
				xl.Warn("從工作連線讀取 UDP 失敗: %v", errRet)
				return
			}
			if errRet := errors.PanicToError(func() {
				xl.Trace("從工作連線取得 UDP 封包: %s", udpMsg.Content)
				readCh <- &udpMsg
			}); errRet != nil {
				xl.Info("UDP 工作連線讀取協程已關閉: %v", errRet)
				return
			}
		}
	}
	workConnSenderFn := func(conn net.Conn, sendCh chan msg.Message) {
		defer func() {
			xl.Info("UDP 工作連線寫入協程已關閉")
		}()
		var errRet error
		for rawMsg := range sendCh {
			switch m := rawMsg.(type) {
			case *msg.UDPPacket:
				xl.Trace("傳送 UDP 封包到工作連線: %s", m.Content)
			case *msg.Ping:
				xl.Trace("傳送心跳訊息到 UDP 工作連線")
			}
			if errRet = msg.WriteMsg(conn, rawMsg); errRet != nil {
				xl.Error("UDP 工作連線寫入失敗: %v", errRet)
				return
			}
		}
	}
	heartbeatFn := func(sendCh chan msg.Message) {
		var errRet error
		for {
			time.Sleep(time.Duration(30) * time.Second)
			if errRet = errors.PanicToError(func() {
				sendCh <- &msg.Ping{}
			}); errRet != nil {
				xl.Trace("UDP 工作連線心跳協程已關閉")
				break
			}
		}
	}

	go workConnSenderFn(pxy.workConn, pxy.sendCh)
	go workConnReaderFn(pxy.workConn, pxy.readCh)
	go heartbeatFn(pxy.sendCh)
	udp.Forwarder(pxy.localAddr, pxy.readCh, pxy.sendCh, int(pxy.clientCfg.UDPPacketSize))
}

type SUDPProxy struct {
	*BaseProxy

	cfg *config.SUDPProxyConf

	localAddr *net.UDPAddr

	closeCh chan struct{}
}

func (pxy *SUDPProxy) Run() (err error) {
	pxy.localAddr, err = net.ResolveUDPAddr("udp", net.JoinHostPort(pxy.cfg.LocalIP, strconv.Itoa(pxy.cfg.LocalPort)))
	if err != nil {
		return
	}
	return
}

func (pxy *SUDPProxy) Close() {
	pxy.mu.Lock()
	defer pxy.mu.Unlock()
	select {
	case <-pxy.closeCh:
		return
	default:
		close(pxy.closeCh)
	}
}

func (pxy *SUDPProxy) InWorkConn(conn net.Conn, m *msg.StartWorkConn) {
	xl := pxy.xl
	xl.Info("收到新的 SUDP 代理工作連線, %s", conn.RemoteAddr().String())

	var rwc io.ReadWriteCloser = conn
	var err error
	if pxy.limiter != nil {
		rwc = frpIo.WrapReadWriteCloser(limit.NewReader(conn, pxy.limiter), limit.NewWriter(conn, pxy.limiter), func() error {
			return conn.Close()
		})
	}
	if pxy.cfg.UseEncryption {
		rwc, err = frpIo.WithEncryption(rwc, []byte(pxy.clientCfg.Token))
		if err != nil {
			conn.Close()
			xl.Error("建立加密串流失敗: %v", err)
			return
		}
	}
	if pxy.cfg.UseCompression {
		rwc = frpIo.WithCompression(rwc)
	}
	conn = frpNet.WrapReadWriteCloserToConn(rwc, conn)

	workConn := conn
	readCh := make(chan *msg.UDPPacket, 1024)
	sendCh := make(chan msg.Message, 1024)
	isClose := false

	mu := &sync.Mutex{}

	closeFn := func() {
		mu.Lock()
		defer mu.Unlock()
		if isClose {
			return
		}

		isClose = true
		if workConn != nil {
			workConn.Close()
		}
		close(readCh)
		close(sendCh)
	}

	// udp 服務 <- frpc <- frps <- frpc 訪客 <- 使用者
	workConnReaderFn := func(conn net.Conn, readCh chan *msg.UDPPacket) {
		defer closeFn()

		for {
			// 先確認 sudp 代理是否已關閉
			select {
			case <-pxy.closeCh:
				xl.Trace("frpc sudp 代理已關閉")
				return
			default:
			}

			var udpMsg msg.UDPPacket
			if errRet := msg.ReadMsgInto(conn, &udpMsg); errRet != nil {
				xl.Warn("從工作連線讀取 sudp 失敗: %v", errRet)
				return
			}

			if errRet := errors.PanicToError(func() {
				readCh <- &udpMsg
			}); errRet != nil {
				xl.Warn("sudp 工作連線讀取協程已關閉: %v", errRet)
				return
			}
		}
	}

	// udp 服務 -> frpc -> frps -> frpc 訪客 -> 使用者
	workConnSenderFn := func(conn net.Conn, sendCh chan msg.Message) {
		defer func() {
			closeFn()
			xl.Info("sudp 工作連線寫入協程已關閉")
		}()

		var errRet error
		for rawMsg := range sendCh {
			switch m := rawMsg.(type) {
			case *msg.UDPPacket:
				xl.Trace("frpc 傳送 UDP 封包到 frpc 訪客，[udp 本地: %v，遠端: %v]，[tcp 工作連線本地: %v，遠端: %v]",
					m.LocalAddr.String(), m.RemoteAddr.String(), conn.LocalAddr().String(), conn.RemoteAddr().String())
			case *msg.Ping:
				xl.Trace("frpc 傳送心跳訊息到 frpc 訪客")
			}

			if errRet = msg.WriteMsg(conn, rawMsg); errRet != nil {
				xl.Error("sudp 工作連線寫入失敗: %v", errRet)
				return
			}
		}
	}

	heartbeatFn := func(sendCh chan msg.Message) {
		ticker := time.NewTicker(30 * time.Second)
		defer func() {
			ticker.Stop()
			closeFn()
		}()

		var errRet error
		for {
			select {
			case <-ticker.C:
				if errRet = errors.PanicToError(func() {
					sendCh <- &msg.Ping{}
				}); errRet != nil {
					xl.Warn("sudp 工作連線心跳協程已關閉")
					return
				}
			case <-pxy.closeCh:
				xl.Trace("frpc sudp 代理已關閉")
				return
			}
		}
	}

	go workConnSenderFn(workConn, sendCh)
	go workConnReaderFn(workConn, readCh)
	go heartbeatFn(sendCh)

	udp.Forwarder(pxy.localAddr, readCh, sendCh, int(pxy.clientCfg.UDPPacketSize))
}

// TCP 工作連線的共用處理流程。
func HandleTCPWorkConnection(ctx context.Context, localInfo *config.LocalSvrConf, proxyPlugin plugin.Plugin,
	baseInfo *config.BaseProxyConf, limiter *rate.Limiter, workConn net.Conn, encKey []byte, m *msg.StartWorkConn,
) {
	xl := xlog.FromContextSafe(ctx)
	var (
		remote io.ReadWriteCloser
		err    error
	)
	remote = workConn
	if limiter != nil {
		remote = frpIo.WrapReadWriteCloser(limit.NewReader(workConn, limiter), limit.NewWriter(workConn, limiter), func() error {
			return workConn.Close()
		})
	}

	xl.Trace("處理 TCP 工作連線，使用加密: %t，使用壓縮: %t",
		baseInfo.UseEncryption, baseInfo.UseCompression)
	if baseInfo.UseEncryption {
		remote, err = frpIo.WithEncryption(remote, encKey)
		if err != nil {
			workConn.Close()
			xl.Error("建立加密串流失敗: %v", err)
			return
		}
	}
	if baseInfo.UseCompression {
		remote = frpIo.WithCompression(remote)
	}

	// 檢查是否需要送出代理協議資訊
	var extraInfo []byte
	if baseInfo.ProxyProtocolVersion != "" {
		if m.SrcAddr != "" && m.SrcPort != 0 {
			if m.DstAddr == "" {
				m.DstAddr = "127.0.0.1"
			}
			srcAddr, _ := net.ResolveTCPAddr("tcp", net.JoinHostPort(m.SrcAddr, strconv.Itoa(int(m.SrcPort))))
			dstAddr, _ := net.ResolveTCPAddr("tcp", net.JoinHostPort(m.DstAddr, strconv.Itoa(int(m.DstPort))))
			h := &pp.Header{
				Command:         pp.PROXY,
				SourceAddr:      srcAddr,
				DestinationAddr: dstAddr,
			}

			if strings.Contains(m.SrcAddr, ".") {
				h.TransportProtocol = pp.TCPv4
			} else {
				h.TransportProtocol = pp.TCPv6
			}

			if baseInfo.ProxyProtocolVersion == "v1" {
				h.Version = 1
			} else if baseInfo.ProxyProtocolVersion == "v2" {
				h.Version = 2
			}

			buf := bytes.NewBuffer(nil)
			_, _ = h.WriteTo(buf)
			extraInfo = buf.Bytes()
		}
	}

	if proxyPlugin != nil {
		// 如果設定了外掛，先交由外掛處理連線
		xl.Debug("由外掛處理: %s", proxyPlugin.Name())
		proxyPlugin.Handle(remote, workConn, extraInfo)
		xl.Debug("外掛處理完成")
		return
	}

	localConn, err := libdial.Dial(
		net.JoinHostPort(localInfo.LocalIP, strconv.Itoa(localInfo.LocalPort)),
		libdial.WithTimeout(10*time.Second),
	)
	if err != nil {
		workConn.Close()
		xl.Error("連線本地服務 [%s:%d] 失敗：%v。請檢查伺服器是否已啟動。", localInfo.LocalIP, localInfo.LocalPort, err)
		return
	}

	xl.Debug("橋接連線，本地連線(l[%s] r[%s]) 工作連線(l[%s] r[%s])", localConn.LocalAddr().String(),
		localConn.RemoteAddr().String(), workConn.LocalAddr().String(), workConn.RemoteAddr().String())

	if len(extraInfo) > 0 {
		if _, err := localConn.Write(extraInfo); err != nil {
			workConn.Close()
			xl.Error("寫入 extraInfo 到本地連線失敗: %v", err)
			return
		}
	}

	frpIo.Join(localConn, remote)
	xl.Debug("連線橋接已關閉")
}
