package launcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	appName   = "TaiwanFRP Launcher"
	version   = "2.0.0"
	copyright = "Copyright (c) 2025 TaiwanFRP"

	infoDir  = "lib"
	infoFile = infoDir + "/info.json"

	serverURL = "https://taiwanfrp.ddns.net"
)

var ErrBypass = errors.New("launcher bypassed")
var ErrLogout = errors.New("launcher logout")

type infoFileData struct {
	Username   string      `json:"username"`
	Password   string      `json:"password"`
	DNSServers []string    `json:"dns_servers,omitempty"`
	Selected   []selection `json:"selected"`
}

type legacyInfo struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Node     struct {
		Name string `json:"name"`
	} `json:"node"`
	Tunnels []string `json:"tunnels"`
}

type selection struct {
	Node  string `json:"node"`
	Proxy string `json:"proxy"`
}

type nodesResponse struct {
	Nodes []nodeInfo `json:"nodes"`
}

type nodeInfo struct {
	Name          string `json:"name"`
	FrpcIniFolder string `json:"frpcIniFolder"`
	IP            string `json:"ip"`
}

// Run executes the launcher flow. It returns ErrBypass if the caller should
// continue to the normal frpc execution path.
func Run(args []string) error {
	if err := configureLauncherDNS(nil); err != nil {
		return fmt.Errorf("DNS 初始化失敗: %w", err)
	}
	if exitCode := parseArgs(args); exitCode != -1 {
		if exitCode == 0 {
			return ErrBypass
		}
		if exitCode == 2 {
			return nil
		}
		return fmt.Errorf("launcher exit code %d", exitCode)
	}

	fmt.Println("歡迎使用 TaiwanFRP 客戶端！")
	fmt.Println("正在啟動啟動器，請稍候...")

	if guiAvailable() && !hasArg(args, "-nogui") {
		if err := runGUI(); err != nil {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(infoDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", infoDir, err)
	}

	for {
		info, err := loadInfoIfExists(infoFile)
		if err != nil {
			return err
		}
		if err := configureLauncherDNS(info.DNSServers); err != nil {
			return fmt.Errorf("dns_servers 設定錯誤: %w", err)
		}

		if err := verifyLogin(info); err != nil {
			fmt.Println("自動登入失敗，請重新輸入帳號密碼。")
			info, err = promptAndSaveInfo(infoFile)
			if err != nil {
				return err
			}
			if err := verifyLogin(info); err != nil {
				return err
			}
		}
		fmt.Printf("登入成功，當前使用者 %s\n", info.Username)

		nodes, err := fetchNodes()
		if err != nil {
			return err
		}

		node2iniContent := map[string]string{}
		node2proxies := map[string][]string{}
		for _, node := range nodes {
			iniContent, err := downloadFrpcIni(node, info.Username, info.Password)
			if err != nil {
				continue
			}
			if !strings.Contains(iniContent, "[common]") {
				continue
			}
			node2iniContent[node.Name] = iniContent
			proxies := parseTunnels(iniContent)
			if len(proxies) > 0 {
				node2proxies[node.Name] = proxies
			}
		}

		if len(node2proxies) == 0 {
			return fmt.Errorf("沒有任何節點有可用代理")
		}

		info.Selected = fixSelected(info.Selected, node2proxies, infoFile)

		edit := false
		var firstInput string
		if len(info.Selected) > 0 {
			idx2np, _ := buildIndex(nodes, node2proxies)
			printProxyListInOrder(nodes, node2proxies, idx2np, info.Selected)
			fmt.Println("請選擇要啟動的代理(前面數字代碼)（多選用逗號分隔，輸入數字切換選擇，all 全選(新手建議)，stop 啟動）")
			fmt.Println("10 秒內輸入代理編號來新增啟用或停用，或按enter直接啟動，或輸入logout來登出，若10秒沒有反應將直接啟動...")
			if input, ok := readLineWithTimeout(10 * time.Second); ok && strings.TrimSpace(input) != "" {
				edit = true
				firstInput = input
			}
		}

		if len(info.Selected) == 0 || edit {
			selected, err := selectProxiesInOrder(nodes, node2proxies, info.Selected, firstInput, len(info.Selected) > 0)
			if err != nil {
				if err == ErrLogout {
					continue
				}
				return err
			}
			if len(selected) == 0 {
				return fmt.Errorf("請至少選擇一個代理")
			}
			info.Selected = selected
			if err := saveInfo(infoFile, info); err != nil {
				return err
			}
		}

		nodeSelected := map[string][]string{}
		for _, sel := range info.Selected {
			nodeSelected[sel.Node] = append(nodeSelected[sel.Node], sel.Proxy)
		}

		frpcPath, err := findFrpcBinary()
		if err != nil {
			return err
		}

		manager, err := startFrpcProcesses(frpcPath, nodes, node2iniContent, nodeSelected, nil)
		if err != nil {
			return err
		}
		fmt.Println("所有 frpc 已啟動，按 Ctrl+C 結束所有代理...")
		waitForSignal()
		manager.Stop()
		manager.Wait()
		fmt.Println("所有 frpc 已結束，客戶端退出。")
		return nil
	}
}

func parseArgs(args []string) int {
	if len(args) == 1 {
		return -1
	}
	for _, arg := range args[1:] {
		if arg == "--no-launcher" {
			return 0
		}
		if arg == "-nogui" || arg == "--nogui" {
			continue
		}
	}
	switch args[1] {
	case "-h", "--help":
		fmt.Printf("Usage: %s [options]\nOptions:\n  -h, --help        Show help\n  -v, --version     Show version\n  --no-launcher     Skip launcher\n", args[0])
		return 2
	case "-v", "--version":
		fmt.Printf("%s %s\n%s\n", appName, version, copyright)
		return 2
	case "-nogui", "--nogui":
		return -1
	default:
		if strings.HasPrefix(args[1], "-") {
			fmt.Printf("Unknown option: %s\nUse -h for help\n", args[1])
			return 1
		}
		return -1
	}
}

func hasArg(args []string, needle string) bool {
	for _, a := range args[1:] {
		if a == needle {
			return true
		}
	}
	return false
}

func loadInfoIfExists(path string) (infoFileData, error) {
	if _, err := os.Stat(path); err == nil {
		fmt.Println("自動登入中...")
		return loadInfo(path)
	}
	return promptAndSaveInfo(path)
}

func promptAndSaveInfo(path string) (infoFileData, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("請輸入使用者名稱: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)
	fmt.Print("密碼(不會顯示): ")
	pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return infoFileData{}, err
	}
	password := strings.TrimSpace(string(pwBytes))

	info := infoFileData{
		Username: username,
		Password: password,
	}
	if err := saveInfo(path, info); err != nil {
		return info, err
	}
	fmt.Println("已經保存登入資料，如要登出請刪除 lib 資料夾。")
	return info, nil
}

func loadInfo(path string) (infoFileData, error) {
	var info infoFileData
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&info); err != nil {
		return info, err
	}
	if len(info.Selected) == 0 {
		if legacy, ok := tryLoadLegacy(path); ok {
			fmt.Println("正在更新info.json...")
			if legacy.Username != "" {
				info.Username = legacy.Username
			}
			if legacy.Password != "" {
				info.Password = legacy.Password
			}
			if legacy.Node.Name != "" && len(legacy.Tunnels) > 0 {
				for _, t := range legacy.Tunnels {
					info.Selected = append(info.Selected, selection{Node: legacy.Node.Name, Proxy: t})
				}
				_ = saveInfo(path, info)
				fmt.Println("更新兼容完畢")
			}
		}
	}
	return info, nil
}

func tryLoadLegacy(path string) (legacyInfo, bool) {
	var legacy legacyInfo
	data, err := os.ReadFile(path)
	if err != nil {
		return legacy, false
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return legacy, false
	}
	if legacy.Username == "" || legacy.Password == "" || legacy.Node.Name == "" || len(legacy.Tunnels) == 0 {
		return legacy, false
	}
	return legacy, true
}

func saveInfo(path string, info infoFileData) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(info)
}

func verifyLogin(info infoFileData) error {
	body := fmt.Sprintf(`{"username":"%s","password":"%s"}`, info.Username, info.Password)
	resp, err := http.Post(serverURL+"/login", "application/json", strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("登入失敗，網路錯誤: %v (DNS: %s)", err, dnsResolverDebugInfo())
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("登入失敗，無效的帳號密碼")
	}
	return fmt.Errorf("登入失敗，HTTP %d (DNS: %s)", resp.StatusCode, dnsResolverDebugInfo())
}

func fetchNodes() ([]nodeInfo, error) {
	resp, err := http.Get(serverURL + "/nodes.json")
	if err != nil {
		return nil, fmt.Errorf("無法取得節點列表: %v (DNS: %s)", err, dnsResolverDebugInfo())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("無法取得節點列表，HTTP %d (DNS: %s)", resp.StatusCode, dnsResolverDebugInfo())
	}
	var out nodesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("節點列表解析失敗")
	}
	return out.Nodes, nil
}

func downloadFrpcIni(node nodeInfo, username, password string) (string, error) {
	iniURL := fmt.Sprintf("%s/%s/%s/frpc.ini", serverURL, node.FrpcIniFolder, username)
	req, err := http.NewRequest("GET", iniURL, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(username, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("下載 frpc.ini 失敗: %v (DNS: %s)", err, dnsResolverDebugInfo())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ini not found")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func parseTunnels(content string) []string {
	lines := strings.Split(content, "\n")
	var tunnels []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && line != "[common]" {
			tunnels = append(tunnels, strings.Trim(line, "[]"))
		}
	}
	return tunnels
}

func fixSelected(selected []selection, node2proxies map[string][]string, infoPath string) []selection {
	if len(selected) == 0 {
		return selected
	}
	var newSelected []selection
	var movedMsgs []string
	for _, sel := range selected {
		if proxyExists(node2proxies, sel.Node, sel.Proxy) {
			newSelected = append(newSelected, sel)
			continue
		}
		found := false
		for node, proxies := range node2proxies {
			for _, p := range proxies {
				if p == sel.Proxy {
					newSelected = append(newSelected, selection{Node: node, Proxy: sel.Proxy})
					movedMsgs = append(movedMsgs, fmt.Sprintf("將節點 '%s' 的代理 '%s' 更換到節點 '%s'", sel.Node, sel.Proxy, node))
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}
	if !selectionsEqual(selected, newSelected) {
		info, err := loadInfo(infoPath)
		if err == nil {
			info.Selected = newSelected
			_ = saveInfo(infoPath, info)
		}
		for _, msg := range movedMsgs {
			fmt.Println(msg)
		}
		fmt.Println("已自動刪除 info.json 內的代理選擇。")
	}
	return newSelected
}

func selectionsEqual(a, b []selection) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func proxyExists(node2proxies map[string][]string, node, proxy string) bool {
	proxies, ok := node2proxies[node]
	if !ok {
		return false
	}
	for _, p := range proxies {
		if p == proxy {
			return true
		}
	}
	return false
}

func readLineWithTimeout(timeout time.Duration) (string, bool) {
	ch := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		text, _ := reader.ReadString('\n')
		ch <- strings.TrimSpace(text)
	}()

	secondsLeft := int(timeout / time.Second)
	showCountdown := secondsLeft > 0
	if showCountdown {
		fmt.Printf("\r\033[2K%d秒後啟動代理...", secondsLeft)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case text := <-ch:
			if showCountdown {
				fmt.Println()
			}
			return text, true
		case <-ticker.C:
			if showCountdown {
				secondsLeft--
				if secondsLeft > 0 {
					fmt.Printf("\r\033[2K%d秒後啟動代理...", secondsLeft)
				}
			}
		case <-timer.C:
			if showCountdown {
				fmt.Println()
			}
			return "", false
		}
	}
}

func selectProxiesInOrder(nodes []nodeInfo, node2proxies map[string][]string, existing []selection, firstInput string, skipInitialPrint bool) ([]selection, error) {
	idx2np, total := buildIndex(nodes, node2proxies)

	selected := make([]selection, 0, len(existing))
	selected = append(selected, existing...)

	reader := bufio.NewReader(os.Stdin)
	firstShow := !skipInitialPrint
	for {
		if firstShow {
			printProxyListInOrder(nodes, node2proxies, idx2np, selected)
			fmt.Println("輸入節點代碼來選擇節點，再次輸入是取消選擇（多選用逗號分隔，輸入數字切換選擇，all 全選，stop 啟動）")
			firstShow = false
		}
		input := ""
		if firstInput != "" {
			input = strings.TrimSpace(firstInput)
			firstInput = ""
		} else {
			input, _ = reader.ReadString('\n')
			input = strings.TrimSpace(input)
		}
		if input == "" {
			fmt.Println("請輸入正確的代理代碼")
			continue
		}
		if input == "logout" {
			return nil, logoutAndRestart()
		}
		if input == "all" {
			selected = selected[:0]
			for i := 1; i <= total; i++ {
				selected = append(selected, idx2np[i])
			}
		} else if input == "stop" {
			if len(selected) == 0 {
				fmt.Println("請至少選擇一個代理")
				continue
			}
			break
		} else {
			valid := false
			for _, token := range strings.Split(input, ",") {
				token = strings.TrimSpace(token)
				i, err := strconv.Atoi(token)
				if err != nil {
					continue
				}
				np, ok := idx2np[i]
				if !ok {
					continue
				}
				valid = true
				if containsSelection(selected, np) {
					selected = removeSelection(selected, np)
				} else {
					selected = append(selected, np)
				}
			}
			if !valid {
				fmt.Println("請輸入正確的代理代碼")
				continue
			}
		}
		printProxyListInOrder(nodes, node2proxies, idx2np, selected)
		fmt.Println("請選擇要啟動的代理（多選用逗號分隔，輸入數字切換選擇，all 全選(新手建議)，stop 啟動）")
	}
	return selected, nil
}

func logoutAndRestart() error {
	_ = os.RemoveAll(infoDir)
	fmt.Println("已登出，請重新登入。")
	return ErrLogout
}

func buildIndex(nodes []nodeInfo, node2proxies map[string][]string) (map[int]selection, int) {
	idx2np := map[int]selection{}
	idx := 1
	for _, node := range nodes {
		proxies := node2proxies[node.Name]
		for _, proxy := range proxies {
			idx2np[idx] = selection{Node: node.Name, Proxy: proxy}
			idx++
		}
	}
	return idx2np, idx - 1
}

func printProxyListInOrder(nodes []nodeInfo, node2proxies map[string][]string, idx2np map[int]selection, selected []selection) {
	for _, node := range nodes {
		proxies := node2proxies[node.Name]
		if len(proxies) == 0 {
			continue
		}
		fmt.Printf("%s：\n", node.Name)
		for _, proxy := range proxies {
			globalIdx := -1
			for i, np := range idx2np {
				if np.Node == node.Name && np.Proxy == proxy {
					globalIdx = i
					break
				}
			}
			fmt.Printf("  %d) %s", globalIdx, proxy)
			if containsSelection(selected, selection{Node: node.Name, Proxy: proxy}) {
				fmt.Print(" [已選擇]")
			}
			fmt.Println()
		}
	}
}

func containsSelection(list []selection, sel selection) bool {
	for _, s := range list {
		if s == sel {
			return true
		}
	}
	return false
}

func removeSelection(list []selection, sel selection) []selection {
	out := list[:0]
	for _, s := range list {
		if s != sel {
			out = append(out, s)
		}
	}
	return out
}

func findFrpcBinary() (string, error) {
	if v := os.Getenv("FRPC_BIN"); v != "" {
		return v, nil
	}
	exe, err := os.Executable()
	if err == nil && exe != "" {
		// Prefer current executable to avoid path lookup issues (supports renamed binaries).
		return exe, nil
	}
	// Fallback to argv[0] if os.Executable fails (common on some Windows setups).
	if os.Args != nil && len(os.Args) > 0 && os.Args[0] != "" {
		if abs, err := filepath.Abs(os.Args[0]); err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				return abs, nil
			}
		}
		if p, err := exec.LookPath(os.Args[0]); err == nil {
			return p, nil
		}
	}

	name := "frpc"
	if runtime.GOOS == "windows" {
		name = "frpc.exe"
	}
	if _, err := os.Stat(name); err == nil {
		return filepath.Abs(name)
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("找不到 frpc，可設定環境變數 FRPC_BIN 指定路徑")
}

type FrpcManager struct {
	cmds   []*exec.Cmd
	wg     sync.WaitGroup
	done   chan struct{}
	cancel context.CancelFunc
}

func (m *FrpcManager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	for _, cmd := range m.cmds {
		_ = terminateProcess(cmd)
	}
}

func (m *FrpcManager) Wait() {
	for _, cmd := range m.cmds {
		_ = cmd.Wait()
	}
	m.wg.Wait()
	if m.done != nil {
		close(m.done)
	}
}

func startFrpcProcesses(frpc string, nodes []nodeInfo, node2iniContent map[string]string, nodeSelected map[string][]string, logFn func(node, line string)) (*FrpcManager, error) {
	if len(nodeSelected) == 0 {
		return nil, fmt.Errorf("沒有可啟動的代理")
	}

	ctx, cancel := context.WithCancel(context.Background())
	childDNSServer := launcherDNSServerForChild()

	manager := &FrpcManager{
		done:   make(chan struct{}),
		cancel: cancel,
	}

	for _, node := range nodes {
		proxies, ok := nodeSelected[node.Name]
		if !ok {
			continue
		}
		srcIniContent, ok := node2iniContent[node.Name]
		if !ok {
			continue
		}
		dstIni := filepath.Join(infoDir, "frpc_"+node.Name+".ini")
		if err := writeIniFromContent(srcIniContent, dstIni, proxies); err != nil {
			manager.Stop()
			return nil, err
		}

		cmd := exec.CommandContext(ctx, frpc, "-c", dstIni)
		env := append(os.Environ(), "TAIWANFRP_SKIP_LAUNCHER=1")
		if childDNSServer != "" {
			env = append(env, "TAIWANFRP_DNS_SERVER="+childDNSServer)
		}
		cmd.Env = env
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()

		if err := cmd.Start(); err != nil {
			manager.Stop()
			return nil, err
		}
		fmt.Printf("[%s] frpc 啟動成功 (PID: %d)\n", node.Name, cmd.Process.Pid)

		manager.wg.Add(2)
		go streamWithPrefix(stdout, node.Name, logFn, &manager.wg)
		go streamWithPrefix(stderr, node.Name, logFn, &manager.wg)
		manager.cmds = append(manager.cmds, cmd)
	}

	return manager, nil
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func terminateProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		return cmd.Process.Kill()
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-time.After(2 * time.Second):
		return cmd.Process.Kill()
	case err := <-done:
		return err
	}
}

func streamWithPrefix(r io.ReadCloser, prefix string, logFn func(node, line string), wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if logFn != nil {
			logFn(prefix, line)
		} else {
			fmt.Printf("[%s] %s\n", prefix, line)
		}
	}
}

func writeIniFromContent(content, dst string, proxies []string) error {
	lines := strings.Split(content, "\n")
	var out bytes.Buffer
	inCommon := false
	inTunnel := false
	for _, line := range lines {
		if line == "[common]" {
			inCommon = true
			inTunnel = false
			out.WriteString(line + "\n")
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.Trim(line, "[]")
			inCommon = false
			inTunnel = containsString(proxies, name)
			if inTunnel {
				out.WriteString("\n" + line + "\n")
			}
			continue
		}
		if inCommon || inTunnel {
			out.WriteString(line + "\n")
		}
	}
	return os.WriteFile(dst, out.Bytes(), 0644)
}

func containsString(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}
