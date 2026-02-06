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
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
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

// Service is a client service.
type Service struct {
	// uniq id got from frps, attach it in loginMsg
	runID string

	// manager control connection with server
	ctl   *Control
	ctlMu sync.RWMutex

	// Sets authentication based on selected method
	authSetter auth.Setter

	cfg         config.ClientCommonConf
	pxyCfgs     map[string]config.ProxyConf
	visitorCfgs map[string]config.VisitorConf
	cfgMu       sync.RWMutex

	// The configuration file used to initialize this client, or an empty
	// string if no configuration file was used.
	cfgFile string

	// This is configured by the login response from frps
	serverUDPPort int

	exit uint32 // 0 means not exit

	// service context
	ctx context.Context
	// call cancel to stop service
	cancel context.CancelFunc
}

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

func (svr *Service) GetController() *Control {
	svr.ctlMu.RLock()
	defer svr.ctlMu.RUnlock()
	return svr.ctl
}

func (svr *Service) Run() error {
	xl := xlog.FromContextSafe(svr.ctx)

	// Execute script before starting the service unless launcher already handled it
	if os.Getenv("TAIWANFRP_SKIP_LAUNCHER") != "1" {
		err := svr.runScript()
		if err != nil {
			xl.Warn("failed to execute script: %v", err)
			return err
		}
	}

	// set custom DNSServer
	if svr.cfg.DNSServer != "" {
		dnsAddr := svr.cfg.DNSServer
		if _, _, err := net.SplitHostPort(dnsAddr); err != nil {
			dnsAddr = net.JoinHostPort(dnsAddr, "53")
		}
		// Change default dns server for frpc
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return net.Dial("udp", dnsAddr)
			},
		}
	}

	// login to frps
	for {
		conn, cm, err := svr.login()
		if err != nil {
			xl.Warn("login to server failed: %v", err)

			// if login_fail_exit is true, just exit this program
			// otherwise sleep a while and try again to connect to server
			if svr.cfg.LoginFailExit {
				return err
			}
			util.RandomSleep(10*time.Second, 0.9, 1.1)
		} else {
			// login success
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
		// Init admin server assets
		assets.Load(svr.cfg.AssetsDir)

		address := net.JoinHostPort(svr.cfg.AdminAddr, strconv.Itoa(svr.cfg.AdminPort))
		err := svr.RunAdminServer(address)
		if err != nil {
			log.Warn("run admin server error: %v", err)
		}
		log.Info("admin server listen on %s:%d", svr.cfg.AdminAddr, svr.cfg.AdminPort)
	}
	<-svr.ctx.Done()
	return nil
}

func (svr *Service) runScript() error {
	// Create lib directory if it doesn't exist
	infoDir := "lib"
	if _, err := os.Stat(infoDir); os.IsNotExist(err) {
		if err := os.Mkdir(infoDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %v", err)
		}
	}

	// Check if jq is installed
	jqFile := infoDir + "/jq"
	if _, err := os.Stat(jqFile); os.IsNotExist(err) {
		fmt.Println("jq 未安裝，正在安裝...")
		if err := installJQ(jqFile); err != nil {
			return fmt.Errorf("failed to install jq: %v", err)
		}
	}

	// Check if info.json exists
	infoFile := infoDir + "/info.json"
	if _, err := os.Stat(infoFile); os.IsNotExist(err) {
		fmt.Println("請輸入使用者名稱和密碼。")
		var username, password string
		fmt.Print("使用者名稱: ")
		fmt.Scanln(&username)
		fmt.Print("密碼: ")
		fmt.Scanln(&password)

		data := map[string]interface{}{
			"username": username,
			"password": password,
		}
		if err := saveJSON(infoFile, data); err != nil {
			return fmt.Errorf("failed to save login data: %v", err)
		}
		fmt.Println("已經保存登入資料，如要登出請刪除info.json。")
	}

	fmt.Println("自動登入中...。")
	return loginAndSetup(infoFile, infoDir)
}

func installJQ(jqFile string) error {
	var jqURL string
	switch runtime.GOOS {
	case "windows":
		jqURL = "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-win64.exe"
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			jqURL = "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-linux64"
		case "386":
			jqURL = "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-linux32"
		case "arm":
			jqURL = "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-linux-arm"
		case "arm64":
			jqURL = "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-linux-arm64"
		default:
			return fmt.Errorf("unsupported linux architecture: %s", runtime.GOARCH)
		}
	case "darwin":
		jqURL = "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-osx-amd64"
	default:
		return fmt.Errorf("unsupported platform")
	}

	if err := downloadFile(jqFile, jqURL); err != nil {
		return err
	}
	return os.Chmod(jqFile, 0755)
}

func loginAndSetup(infoFile, infoDir string) error {
	data, err := loadJSON(infoFile)
	if err != nil {
		return fmt.Errorf("failed to load info file: %v", err)
	}
	username := data["username"]
	password := data["password"]

	// Verify server connection
	serverURL := "https://taiwanfrp.ddns.net"
	resp, err := http.Post(serverURL+"/login", "application/json", strings.NewReader(fmt.Sprintf(`{"username":"%s","password":"%s"}`, username, password)))
	if err != nil {
		return fmt.Errorf("failed to login: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		fmt.Println("登入成功")

		// Remove existing frpc.ini and frpcrun
		frpcIniFile := infoDir + "/frpc.ini"
		frpcRunFile := infoDir + "/frpcrun"
		os.Remove(frpcIniFile)
		os.Remove(frpcRunFile)

		// Read frpc.ini content from server
		frpcIniContent, err := downloadContent(fmt.Sprintf("%s/frpcini/%s/frpc.ini", serverURL, username))
		if err != nil {
			return fmt.Errorf("failed to download frpc.ini: %v", err)
		}

		// Check if tunnels are already saved in info.json
		savedTunnels, ok := data["tunnels"].([]interface{})
		if !ok {
			// Parse tunnels from frpc.ini content and let user select
			tunnels := parseTunnels(frpcIniContent)
			selectedTunnels := selectTunnels(tunnels)
			data["tunnels"] = selectedTunnels
			if err := saveJSON(infoFile, data); err != nil {
				return fmt.Errorf("failed to save tunnels: %v", err)
			}
		} else {
			selectedTunnels := validateTunnels(savedTunnels, frpcIniContent)
			data["tunnels"] = selectedTunnels
			if err := saveJSON(infoFile, data); err != nil {
				return fmt.Errorf("failed to save tunnels: %v", err)
			}
		}

		// Filter frpc.ini content based on selected tunnels
		filteredContent := filterTunnels(frpcIniContent, data["tunnels"].([]interface{}))
		if err := ioutil.WriteFile(frpcIniFile, []byte(filteredContent), 0644); err != nil {
			return fmt.Errorf("failed to write frpc.ini: %v", err)
		}

		// Download frpcrun
		if err := downloadFile(frpcRunFile, fmt.Sprintf("%s/windows/frpcrun.exe", serverURL)); err != nil {
			return fmt.Errorf("failed to download frpcrun: %v", err)
		}
		if err := os.Chmod(frpcRunFile, 0755); err != nil {
			return fmt.Errorf("failed to set frpcrun permissions: %v", err)
		}

		// Run frpcrun
		cmd := exec.Command(frpcRunFile, "-c", frpcIniFile)
		cmd.Dir = infoDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	} else if resp.StatusCode == http.StatusBadRequest {
		fmt.Println("登入失敗，無效的帳號密碼。")
		os.Remove(infoFile)
	} else {
		fmt.Println("登入失敗，檢查網路連接。")
	}
	return nil
}

func downloadFile(filepath string, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func downloadContent(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	return string(body), err
}

func saveJSON(filepath string, data map[string]interface{}) error {
	file, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(data)
}

func loadJSON(filepath string) (map[string]interface{}, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var data map[string]interface{}
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&data)
	return data, err
}

func parseTunnels(content string) []string {
	lines := strings.Split(content, "\n")
	var tunnels []string
	for _, line := range lines {
		if strings.HasPrefix(line, "[") && !strings.HasPrefix(line, "[common]") {
			tunnels = append(tunnels, strings.Trim(line, "[]"))
		}
	}
	return tunnels
}

func selectTunnels(tunnels []string) []string {
	var selectedTunnels []string
	for {
		fmt.Printf("當前已選擇代理: %v\n", selectedTunnels)
		fmt.Println("可用代理:")
		for i, tunnel := range tunnels {
			fmt.Printf("%d) %s\n", i+1, tunnel)
		}
		fmt.Println("(輸入 'stop' 表示已經選完所有代理, 'all' 表示選擇所有代理, 新手建議直接輸入all):")
		var input string
		fmt.Scanln(&input)
		if input == "stop" || input == "all" {
			if input == "all" {
				selectedTunnels = tunnels
			}
			break
		}
		index, err := strconv.Atoi(input)
		if err != nil || index < 1 || index > len(tunnels) {
			fmt.Println("無效的輸入，請重新輸入。")
			continue
		}
		tunnel := tunnels[index-1]
		if containsString(selectedTunnels, tunnel) {
			selectedTunnels = remove(selectedTunnels, tunnel)
			fmt.Printf("取消選擇代理: %s\n", tunnel)
		} else {
			selectedTunnels = append(selectedTunnels, tunnel)
			fmt.Printf("已選擇代理: %s\n", tunnel)
		}
	}
	return selectedTunnels
}

func validateTunnels(savedTunnels []interface{}, content string) []string {
	tunnels := parseTunnels(content)
	var validTunnels []string
	for _, tunnel := range savedTunnels {
		if containsString(tunnels, tunnel.(string)) {
			validTunnels = append(validTunnels, tunnel.(string))
		} else {
			fmt.Printf("代理 %s 不存在，已自動移除。\n", tunnel)
		}
	}
	return validTunnels
}

func filterTunnels(content string, selectedTunnels []interface{}) string {
	lines := strings.Split(content, "\n")
	var filteredContent []string
	include := false
	for _, line := range lines {
		if strings.HasPrefix(line, "[") {
			section := strings.Trim(line, "[]")
			if section == "common" || contains(selectedTunnels, section) {
				include = true
			} else {
				include = false
			}
		}
		if include {
			filteredContent = append(filteredContent, line)
		}
	}
	return strings.Join(filteredContent, "\n")
}

func contains(slice []interface{}, item string) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}

func containsString(slice []string, item string) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}

func remove(slice []string, item string) []string {
	for i, v := range slice {
		if v == item {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func (svr *Service) keepControllerWorking() {
	xl := xlog.FromContextSafe(svr.ctx)
	maxDelayTime := 20 * time.Second
	delayTime := time.Second

	// if frpc reconnect frps, we need to limit retry times in 1min
	// current retry logic is sleep 0s, 0s, 0s, 1s, 2s, 4s, 8s, ...
	// when exceed 1min, we will reset delay and counts
	cutoffTime := time.Now().Add(time.Minute)
	reconnectDelay := time.Second
	reconnectCounts := 1

	for {
		<-svr.ctl.ClosedDoneCh()
		if atomic.LoadUint32(&svr.exit) != 0 {
			return
		}

		// the first three retry with no delay
		if reconnectCounts > 3 {
			util.RandomSleep(reconnectDelay, 0.9, 1.1)
			xl.Info("wait %v to reconnect", reconnectDelay)
			reconnectDelay *= 2
		} else {
			util.RandomSleep(time.Second, 0, 0.5)
		}
		reconnectCounts++

		now := time.Now()
		if now.After(cutoffTime) {
			// reset
			cutoffTime = now.Add(time.Minute)
			reconnectDelay = time.Second
			reconnectCounts = 1
		}

		for {
			if atomic.LoadUint32(&svr.exit) != 0 {
				return
			}

			xl.Info("嘗試連接TaiwanFRP節點...")
			conn, cm, err := svr.login()
			if err != nil {
				xl.Warn("reconnect to server error: %v, wait %v for another retry", err, delayTime)
				util.RandomSleep(delayTime, 0.9, 1.1)

				delayTime *= 2
				if delayTime > maxDelayTime {
					delayTime = maxDelayTime
				}
				continue
			}
			// reconnect success, init delayTime
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

// login creates a connection to frps and registers it self as a client
// conn: control connection
// session: if it's not nil, using tcp mux
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

	// Add auth
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
	xl.Info("鄧入伺服器成功, 取得run id [%s], 伺服器udp端口 [%d]", loginRespMsg.RunID, loginRespMsg.ServerUDPPort)
	return
}

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

func (svr *Service) Close() {
	svr.GracefulClose(time.Duration(0))
}

func (svr *Service) GracefulClose(d time.Duration) {
	atomic.StoreUint32(&svr.exit, 1)

	svr.ctlMu.RLock()
	if svr.ctl != nil {
		svr.ctl.GracefulClose(d)
	}
	svr.ctlMu.RUnlock()

	svr.cancel()
}

type ConnectionManager struct {
	ctx context.Context
	cfg *config.ClientCommonConf

	muxSession *fmux.Session
	quicConn   quic.Connection
}

func NewConnectionManager(ctx context.Context, cfg *config.ClientCommonConf) *ConnectionManager {
	return &ConnectionManager{
		ctx: ctx,
		cfg: cfg,
	}
}

func (cm *ConnectionManager) OpenConnection() error {
	xl := xlog.FromContextSafe(cm.ctx)

	// special for quic
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
			xl.Warn("fail to build tls configuration, err: %v", err)
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
			xl.Warn("fail to build tls configuration, err: %v", err)
			return nil, err
		}
	}

	proxyType, addr, auth, err := libdial.ParseProxyURL(cm.cfg.HTTPProxy)
	if err != nil {
		xl.Error("fail to parse proxy url")
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

func (cm *ConnectionManager) Close() error {
	if cm.quicConn != nil {
		_ = cm.quicConn.CloseWithError(0, "")
	}
	if cm.muxSession != nil {
		_ = cm.muxSession.Close()
	}
	return nil
}
