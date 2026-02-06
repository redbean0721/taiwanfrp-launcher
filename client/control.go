// 版權所有 2017 fatedier, fatedier@gmail.com
//
// 根據 Apache 許可證 2.0 版（“許可證”）授權；
// 除非遵守許可證，否則您不得使用此文件。
// 您可以在以下位置獲得許可證副本
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// 除非適用法律要求或書面同意，否則根據許可證分發的軟體
// 是按“原樣”分發的，不附帶任何明示或暗示的保證或條件。
// 有關許可證下特定語言的權限和限制，請參閱許可證。

package client

import (
	"context"
	"io"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"github.com/fatedier/golib/control/shutdown"
	"github.com/fatedier/golib/crypto"

	"github.com/fatedier/frp/client/proxy"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/util/xlog"
)

type Control struct {
	// 從 frps 獲取的唯一 ID，附加在 loginMsg 中
	runID string

	// 管理所有代理
	pxyCfgs map[string]config.ProxyConf
	pm      *proxy.Manager

	// 管理所有訪客
	vm *VisitorManager

	// 控制連接
	conn net.Conn

	cm *ConnectionManager

	// 將消息放入此通道以通過控制連接發送到伺服器
	sendCh chan (msg.Message)

	// 從此通道讀取以獲取伺服器發送的下一條消息
	readCh chan (msg.Message)

	// goroutines 可以通過從此通道讀取來阻塞，僅在控制連接關閉時在 reader() 中關閉
	closedCh chan struct{}

	closedDoneCh chan struct{}

	// 最後一次收到 Pong 消息的時間
	lastPong time.Time

	// 客戶端配置
	clientCfg config.ClientCommonConf

	readerShutdown     *shutdown.Shutdown
	writerShutdown     *shutdown.Shutdown
	msgHandlerShutdown *shutdown.Shutdown

	// 伺服器正在監聽的 UDP 埠
	serverUDPPort int

	xl *xlog.Logger

	// 服務上下文
	ctx context.Context

	// 根據選擇的方法設置身份驗證
	authSetter auth.Setter
}

func NewControl(
	ctx context.Context, runID string, conn net.Conn, cm *ConnectionManager,
	clientCfg config.ClientCommonConf,
	pxyCfgs map[string]config.ProxyConf,
	visitorCfgs map[string]config.VisitorConf,
	serverUDPPort int,
	authSetter auth.Setter,
) *Control {
	// 新的 xlog 實例
	ctl := &Control{
		runID:              runID,
		conn:               conn,
		cm:                 cm,
		pxyCfgs:            pxyCfgs,
		sendCh:             make(chan msg.Message, 100),
		readCh:             make(chan msg.Message, 100),
		closedCh:           make(chan struct{}),
		closedDoneCh:       make(chan struct{}),
		clientCfg:          clientCfg,
		readerShutdown:     shutdown.New(),
		writerShutdown:     shutdown.New(),
		msgHandlerShutdown: shutdown.New(),
		serverUDPPort:      serverUDPPort,
		xl:                 xlog.FromContextSafe(ctx),
		ctx:                ctx,
		authSetter:         authSetter,
	}
	ctl.pm = proxy.NewManager(ctl.ctx, ctl.sendCh, clientCfg, serverUDPPort)

	ctl.vm = NewVisitorManager(ctl.ctx, ctl)
	ctl.vm.Reload(visitorCfgs)
	return ctl
}

func (ctl *Control) Run() {
	go ctl.worker()

	// 啟動所有代理
	ctl.pm.Reload(ctl.pxyCfgs)

	// 啟動所有訪客
	go ctl.vm.Run()
}

func (ctl *Control) HandleReqWorkConn(inMsg *msg.ReqWorkConn) {
	xl := ctl.xl
	workConn, err := ctl.connectServer()
	if err != nil {
		xl.Warn("啟動新連接到伺服器錯誤: %v", err)
		return
	}

	m := &msg.NewWorkConn{
		RunID: ctl.runID,
	}
	if err = ctl.authSetter.SetNewWorkConn(m); err != nil {
		xl.Warn("NewWorkConn 身份驗證期間出錯: %v", err)
		return
	}
	if err = msg.WriteMsg(workConn, m); err != nil {
		xl.Warn("工作連接寫入伺服器錯誤: %v", err)
		workConn.Close()
		return
	}

	var startMsg msg.StartWorkConn
	if err = msg.ReadMsgInto(workConn, &startMsg); err != nil {
		xl.Trace("工作連接在響應 StartWorkConn 消息之前關閉: %v", err)
		workConn.Close()
		return
	}
	if startMsg.Error != "" {
		xl.Error("StartWorkConn 包含錯誤: %s", startMsg.Error)
		workConn.Close()
		return
	}

	// 將此工作連接分派給相關代理
	ctl.pm.HandleWorkConn(startMsg.ProxyName, workConn, &startMsg)
}

func (ctl *Control) HandleNewProxyResp(inMsg *msg.NewProxyResp) {
	xl := ctl.xl
	// 伺服器將返回 NewProxyResp 消息給每個 NewProxy 消息。
	// 如果沒有錯誤，啟動新的代理處理程序
	err := ctl.pm.StartProxy(inMsg.ProxyName, inMsg.RemoteAddr, inMsg.Error)
	if err != nil {
		xl.Warn("[%s] 啟動錯誤: %v", inMsg.ProxyName, err)
	} else {
		remoteAddr := inMsg.RemoteAddr
		if strings.HasPrefix(remoteAddr, ":") && ctl.clientCfg.ServerAddr != "" {
			remoteAddr = ctl.clientCfg.ServerAddr + remoteAddr
		}
		if remoteAddr == "" {
			remoteAddr = "N/A"
		}
		xl.Info("[%s] 啟動代理成功，連線位址: %s", inMsg.ProxyName, remoteAddr)
	}
}

func (ctl *Control) Close() error {
	return ctl.GracefulClose(0)
}

func (ctl *Control) GracefulClose(d time.Duration) error {
	ctl.pm.Close()
	ctl.vm.Close()

	time.Sleep(d)

	ctl.conn.Close()
	ctl.cm.Close()
	return nil
}

// ClosedDoneCh 返回一個通道，該通道將在所有資源釋放後關閉
func (ctl *Control) ClosedDoneCh() <-chan struct{} {
	return ctl.closedDoneCh
}

// connectServer 返回到 frps 的新連接
func (ctl *Control) connectServer() (conn net.Conn, err error) {
	return ctl.cm.Connect()
}

// reader 從 frps 讀取所有消息並發送到 readCh
func (ctl *Control) reader() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("恐慌錯誤: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()
	defer ctl.readerShutdown.Done()
	defer close(ctl.closedCh)

	encReader := crypto.NewReader(ctl.conn, []byte(ctl.clientCfg.Token))
	for {
		m, err := msg.ReadMsg(encReader)
		if err != nil {
			if err == io.EOF {
				xl.Debug("從控制連接讀取 EOF")
				return
			}
			xl.Warn("讀取錯誤: %v", err)
			ctl.conn.Close()
			return
		}
		ctl.readCh <- m
	}
}

// writer 將從 sendCh 獲取的消息寫入 frps
func (ctl *Control) writer() {
	xl := ctl.xl
	defer ctl.writerShutdown.Done()
	encWriter, err := crypto.NewWriter(ctl.conn, []byte(ctl.clientCfg.Token))
	if err != nil {
		xl.Error("加密新寫入器錯誤: %v", err)
		ctl.conn.Close()
		return
	}
	for {
		m, ok := <-ctl.sendCh
		if !ok {
			xl.Info("控制寫入器正在關閉")
			return
		}

		if err := msg.WriteMsg(encWriter, m); err != nil {
			xl.Warn("寫入消息到控制連接錯誤: %v", err)
			return
		}
	}
}

// msgHandler 處理所有通道事件並執行相應的操作。
func (ctl *Control) msgHandler() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("恐慌錯誤: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()
	defer ctl.msgHandlerShutdown.Done()

	var hbSendCh <-chan time.Time
	// TODO(fatedier): 如果啟用了 TCPMux，則禁用heart。
	// 只是將其保留在這裡以與舊版本的 frps 保持兼容。
	if ctl.clientCfg.HeartbeatInterval > 0 {
		hbSend := time.NewTicker(time.Duration(ctl.clientCfg.HeartbeatInterval) * time.Second)
		defer hbSend.Stop()
		hbSendCh = hbSend.C
	}

	var hbCheckCh <-chan time.Time
	// 僅在未啟用 TCPMux 且用戶未禁用heart功能時檢查heart超時。
	if ctl.clientCfg.HeartbeatInterval > 0 && ctl.clientCfg.HeartbeatTimeout > 0 && !ctl.clientCfg.TCPMux {
		hbCheck := time.NewTicker(time.Second)
		defer hbCheck.Stop()
		hbCheckCh = hbCheck.C
	}

	ctl.lastPong = time.Now()
	for {
		select {
		case <-hbSendCh:
			// 發送heart到伺服器
			xl.Debug("發送heart到伺服器")
			pingMsg := &msg.Ping{}
			if err := ctl.authSetter.SetPing(pingMsg); err != nil {
				xl.Warn("ping 身份驗證期間出錯: %v", err)
				return
			}
			ctl.sendCh <- pingMsg
		case <-hbCheckCh:
			if time.Since(ctl.lastPong) > time.Duration(ctl.clientCfg.HeartbeatTimeout)*time.Second {
				xl.Warn("heart超時")
				// 讓 reader() 停止
				ctl.conn.Close()
				return
			}
		case rawMsg, ok := <-ctl.readCh:
			if !ok {
				return
			}

			switch m := rawMsg.(type) {
			case *msg.ReqWorkConn:
				go ctl.HandleReqWorkConn(m)
			case *msg.NewProxyResp:
				ctl.HandleNewProxyResp(m)
			case *msg.Pong:
				if m.Error != "" {
					xl.Error("Pong 錯誤訊息: %s", m.Error)
					ctl.conn.Close()
					return
				}
				ctl.lastPong = time.Now()
				xl.Debug("從伺服器接收heart")
			}
		}
	}
}

// 如果控制器被 closedCh 通知，reader 和 writer 以及 handler 將退出
func (ctl *Control) worker() {
	go ctl.msgHandler()
	go ctl.reader()
	go ctl.writer()

	<-ctl.closedCh
	// 關閉相關通道並等待其他 goroutines 完成
	close(ctl.readCh)
	ctl.readerShutdown.WaitDone()
	ctl.msgHandlerShutdown.WaitDone()

	close(ctl.sendCh)
	ctl.writerShutdown.WaitDone()

	ctl.pm.Close()
	ctl.vm.Close()

	close(ctl.closedDoneCh)
	ctl.cm.Close()
}

func (ctl *Control) ReloadConf(pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf) error {
	ctl.vm.Reload(visitorCfgs)
	ctl.pm.Reload(pxyCfgs)
	return nil
}
