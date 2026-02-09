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
	"net/url"
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
)

const (
	infoDir  = "lib"
	infoFile = infoDir + "/info.json"
	iniFile  = infoDir + "/frps.ini"

	serverURL = "https://taiwanfrp.ddns.net"
)

var ErrBypass = errors.New("launcher bypassed")
var ErrLogout = errors.New("launcher logout")

type infoFileData struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Node     struct {
		Name string `json:"name"`
	} `json:"node"`
	SelectedNode string `json:"selected_node,omitempty"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type getNodesResponse struct {
	Approved []serverNode `json:"approved"`
	Pending  []serverNode `json:"pending"`
}

type serverNode struct {
	Name           string `json:"name"`
	IP             string `json:"ip"`
	AvailablePorts string `json:"availablePorts"`
	Owner          string `json:"owner"`
	Status         string `json:"status"`
	Approved       bool   `json:"-"`
}

type generateFrpsIniRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	NodeName string `json:"nodeName"`
}

type processManager struct {
	cmd    *exec.Cmd
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func Run(args []string) error {
	if err := chdirToExecutableDir(); err != nil {
		return err
	}

	if parseArgs(args) {
		return ErrBypass
	}

	fmt.Println("歡迎使用 TaiwanFRP 伺服器啟動器！")
	fmt.Println("正在啟動啟動器，請稍候...")

	if err := os.MkdirAll(infoDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", infoDir, err)
	}

	for {
		info, err := loadInfoIfExists(infoFile)
		if err != nil {
			return err
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

		nodes, err := fetchServerNodes(info.Username, info.Password)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			fmt.Println("目前沒有可用節點，已自動登出。")
			if err := logoutAndRestart(); err != nil {
				return err
			}
			continue
		}

		selected := getSelectedNode(info)
		if !nodeExists(nodes, selected) {
			selected = ""
		}

		if selected != "" {
			printNodeList(nodes, selected)
			fmt.Printf("目前選擇節點: %s\n", selected)
			fmt.Println("10 秒內可輸入節點編號切換，或輸入 logout 登出；直接按 Enter 或逾時將沿用目前節點啟動。")
			if input, ok := readLineWithCountdown(10*time.Second, "若沒有操作，將在"); ok {
				input = strings.TrimSpace(input)
				if input == "logout" {
					if err := logoutAndRestart(); err != nil {
						return err
					}
					continue
				}
				if input != "" {
					selected, err = selectSingleNode(nodes, input, true)
					if err != nil {
						if err == ErrLogout {
							continue
						}
						return err
					}
				}
			}
		}

		if selected == "" {
			selected, err = selectSingleNode(nodes, "", false)
			if err != nil {
				if err == ErrLogout {
					continue
				}
				return err
			}
		}

		setSelectedNode(&info, selected)
		if err := saveInfo(infoFile, info); err != nil {
			return err
		}

		iniContent, err := downloadFrpsIni(info.Username, info.Password, selected)
		if err != nil {
			return err
		}
		if err := os.WriteFile(iniFile, []byte(iniContent), 0644); err != nil {
			return fmt.Errorf("寫入 frps.ini 失敗: %w", err)
		}
		fmt.Printf("已下載 frps.ini 到 %s\n", iniFile)

		frpsPath, err := findFrpsBinary()
		if err != nil {
			return err
		}

		manager, err := startFrpsProcess(frpsPath, iniFile, selected)
		if err != nil {
			return err
		}

		fmt.Printf("[%s] frps 已啟動，按 Ctrl+C 結束伺服器...\n", selected)
		waitForSignal()
		manager.Stop()
		manager.Wait()
		fmt.Println("frps 已結束，伺服器退出。")
		return nil
	}
}

func parseArgs(args []string) bool {
	if len(args) <= 1 {
		return false
	}

	for _, arg := range args[1:] {
		if arg == "--no-launcher" {
			return true
		}
		if strings.HasPrefix(arg, "-") {
			return true
		}
	}
	return false
}

func chdirToExecutableDir() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}
	dir := filepath.Dir(exe)
	if dir == "" {
		return nil
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("failed to change working directory to %s: %w", dir, err)
	}
	return nil
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
	fmt.Print("請輸入密碼(不會顯示): ")
	password, err := readPasswordLine(reader)
	if err != nil {
		return infoFileData{}, err
	}

	info := infoFileData{Username: username, Password: password}
	if err := saveInfo(path, info); err != nil {
		return info, err
	}
	fmt.Println("已經保存登入資料，如要登出請刪除 lib 資料夾。")
	return info, nil
}

func readPasswordLine(reader *bufio.Reader) (string, error) {
	if runtime.GOOS == "windows" {
		pw, err := reader.ReadString('\n')
		return strings.TrimSpace(pw), err
	}

	disableEcho := exec.Command("stty", "-echo")
	disableEcho.Stdin = os.Stdin
	if err := disableEcho.Run(); err != nil {
		pw, readErr := reader.ReadString('\n')
		return strings.TrimSpace(pw), readErr
	}
	defer func() {
		enableEcho := exec.Command("stty", "echo")
		enableEcho.Stdin = os.Stdin
		_ = enableEcho.Run()
	}()

	pw, err := reader.ReadString('\n')
	fmt.Println()
	return strings.TrimSpace(pw), err
}

func loadInfo(path string) (infoFileData, error) {
	var info infoFileData
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(&info); err != nil {
		return info, err
	}
	return info, nil
}

func saveInfo(path string, info infoFileData) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(info)
}

func getSelectedNode(info infoFileData) string {
	if info.Node.Name != "" {
		return info.Node.Name
	}
	return info.SelectedNode
}

func setSelectedNode(info *infoFileData, nodeName string) {
	info.Node.Name = nodeName
	info.SelectedNode = nodeName
}

func verifyLogin(info infoFileData) error {
	payload, _ := json.Marshal(loginRequest{Username: info.Username, Password: info.Password})
	resp, err := http.Post(serverURL+"/login", "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("登入失敗，檢查網路連接")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("登入失敗，無效的帳號密碼")
	}
	return fmt.Errorf("登入失敗，檢查網路連接")
}

func fetchServerNodes(username, password string) ([]serverNode, error) {
	q := url.Values{}
	q.Set("username", username)
	q.Set("password", password)
	resp, err := http.Get(serverURL + "/get_nodes?" + q.Encode())
	if err != nil {
		return nil, fmt.Errorf("無法取得節點列表，請檢查網路")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("無法取得節點列表，請檢查網路")
	}

	var out getNodesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("節點列表解析失敗")
	}

	nodes := make([]serverNode, 0, len(out.Approved)+len(out.Pending))
	for _, n := range out.Approved {
		n.Approved = true
		nodes = append(nodes, n)
	}
	for _, n := range out.Pending {
		n.Approved = false
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func nodeExists(nodes []serverNode, nodeName string) bool {
	if nodeName == "" {
		return false
	}
	for _, n := range nodes {
		if n.Name == nodeName {
			return true
		}
	}
	return false
}

func printNodeList(nodes []serverNode, selected string) {
	for i, node := range nodes {
		review := "未審核"
		if node.Approved {
			review = "已審核"
		}
		fmt.Printf("%d) [%s] %s (IP: %s, 可用端口: %s", i+1, review, node.Name, node.IP, node.AvailablePorts)
		if node.Owner != "" {
			fmt.Printf(", 擁有者: %s", node.Owner)
		}
		fmt.Print(")")
		if node.Status != "" {
			fmt.Printf(" 狀態: %s", node.Status)
		}
		if node.Name == selected {
			fmt.Print(" [已選擇]")
		}
		fmt.Println()
	}
}

func selectSingleNode(nodes []serverNode, firstInput string, skipInitialPrint bool) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	firstShow := !skipInitialPrint

	for {
		if firstShow {
			printNodeList(nodes, "")
			fmt.Println("輸入節點代碼選擇節點（單選），輸入 logout 登出。")
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

		if input == "logout" {
			return "", logoutAndRestart()
		}

		i, err := strconv.Atoi(input)
		if err != nil || i < 1 || i > len(nodes) {
			fmt.Println("請輸入正確的節點代碼")
			continue
		}
		return nodes[i-1].Name, nil
	}
}

func readLineWithCountdown(timeout time.Duration, prefix string) (string, bool) {
	ch := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		text, _ := reader.ReadString('\n')
		ch <- strings.TrimSpace(text)
	}()

	remaining := int(timeout.Seconds())
	if remaining <= 0 {
		return "", false
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	printCountdown := func(sec int) {
		msg := fmt.Sprintf("%s %d 秒後自動啟動...", prefix, sec)
		fmt.Printf("\r%-64s", msg)
	}
	printCountdown(remaining)

	for {
		select {
		case text := <-ch:
			fmt.Print("\r\033[K")
			return text, true
		case <-ticker.C:
			remaining--
			if remaining <= 0 {
				fmt.Print("\r\033[K")
				fmt.Println("倒數結束，自動啟動。")
				return "", false
			}
			printCountdown(remaining)
		}
	}
}

func logoutAndRestart() error {
	if err := os.RemoveAll(infoDir); err != nil {
		return err
	}
	fmt.Println("已登出，請重新登入。")
	return ErrLogout
}

func downloadFrpsIni(username, password, nodeName string) (string, error) {
	payload, _ := json.Marshal(generateFrpsIniRequest{
		Username: username,
		Password: password,
		NodeName: nodeName,
	})

	resp, err := http.Post(serverURL+"/generate_frps_ini", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("無法取得 frps.ini，請檢查網路")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("讀取 frps.ini 失敗")
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = "伺服器回應錯誤"
		}
		return "", fmt.Errorf("無法取得 frps.ini: %s", msg)
	}
	return string(body), nil
}

func findFrpsBinary() (string, error) {
	if v := os.Getenv("FRPS_BIN"); v != "" {
		return v, nil
	}
	exe, err := os.Executable()
	if err == nil && exe != "" {
		return exe, nil
	}

	name := "frps"
	if runtime.GOOS == "windows" {
		name = "frps.exe"
	}
	if _, err := os.Stat(name); err == nil {
		return filepath.Abs(name)
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("找不到 frps，可設定環境變數 FRPS_BIN 指定路徑")
}

func startFrpsProcess(frpsPath, iniPath, nodeName string) (*processManager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, frpsPath, "-c", iniPath)
	cmd.Env = append(os.Environ(), "TAIWANFRP_SKIP_LAUNCHER=1")

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	fmt.Printf("[%s] frps 啟動成功 (PID: %d)\n", nodeName, cmd.Process.Pid)

	m := &processManager{cmd: cmd, cancel: cancel}
	m.wg.Add(2)
	go streamWithPrefix(stdout, nodeName, &m.wg)
	go streamWithPrefix(stderr, nodeName, &m.wg)
	return m, nil
}

func (m *processManager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.cmd != nil && m.cmd.Process != nil {
		_ = terminateProcess(m.cmd)
	}
}

func (m *processManager) Wait() {
	if m.cmd != nil {
		_ = m.cmd.Wait()
	}
	m.wg.Wait()
}

func streamWithPrefix(r io.ReadCloser, prefix string, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		fmt.Printf("[%s] %s\n", prefix, scanner.Text())
	}
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
