package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	infoDir     = "lib"
	infoFile    = infoDir + "/info.json"
	frpcIniFile = infoDir + "/frpc.ini"
	frpcRunFile = infoDir + "/frpcrun"
	jqFile      = infoDir + "/jq"
	serverURL   = "https://taiwanfrp.ddns.net"
)

func main() {
	if err := run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Create lib directory if it doesn't exist
	if _, err := os.Stat(infoDir); os.IsNotExist(err) {
		if err := os.Mkdir(infoDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %v", err)
		}
	}

	// Check if jq is installed
	if _, err := os.Stat(jqFile); os.IsNotExist(err) {
		fmt.Println("jq 未安裝，正在安裝...")
		if err := installJQ(); err != nil {
			return fmt.Errorf("failed to install jq: %v", err)
		}
	}

	if _, err := os.Stat(infoFile); os.IsNotExist(err) {
		fmt.Println("請輸入使用者名稱和密碼。")
		var username, password string
		fmt.Print("使用者名稱: ")
		fmt.Scanln(&username)
		fmt.Print("密碼: ")
		fmt.Scanln(&password)

		data := map[string]string{
			"username": username,
			"password": password,
		}
		if err := saveJSON(infoFile, data); err != nil {
			return fmt.Errorf("failed to save login data: %v", err)
		}
		fmt.Println("已經保存登入資料，如要登出請刪除info.json。")
	}

	fmt.Println("自動登入中...。")
	return loginAndSetup()
}

func installJQ() error {
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

func loginAndSetup() error {
	data, err := loadJSON(infoFile)
	if err != nil {
		return fmt.Errorf("failed to load info file: %v", err)
	}
	username := data["username"]
	password := data["password"]

	// Verify server connection
	resp, err := http.Post(serverURL+"/login", "application/json", strings.NewReader(fmt.Sprintf(`{"username":"%s","password":"%s"}`, username, password)))
	if err != nil {
		return fmt.Errorf("failed to login: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		fmt.Println("登入成功")

		// Remove existing frpc.ini and frpcrun
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
		fmt.Printf("當前已選擇隧道: %v\n", selectedTunnels)
		fmt.Println("可用隧道:")
		for i, tunnel := range tunnels {
			fmt.Printf("%d) %s\n", i+1, tunnel)
		}
		fmt.Println("(輸入 'stop' 表示已經選完所有隧道, 'all' 表示選擇所有隧道, 新手建議直接輸入all):")
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
		if contains(selectedTunnels, tunnel) {
			selectedTunnels = remove(selectedTunnels, tunnel)
			fmt.Printf("取消選擇隧道: %s\n", tunnel)
		} else {
			selectedTunnels = append(selectedTunnels, tunnel)
			fmt.Printf("已選擇隧道: %s\n", tunnel)
		}
	}
	return selectedTunnels
}

func validateTunnels(savedTunnels []interface{}, content string) []string {
	tunnels := parseTunnels(content)
	var validTunnels []string
	for _, tunnel := range savedTunnels {
		if contains(tunnels, tunnel.(string)) {
			validTunnels = append(validTunnels, tunnel.(string))
		} else {
			fmt.Printf("隧道 %s 不存在，已自動移除。\n", tunnel)
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

func remove(slice []string, item string) []string {
	for i, v := range slice {
		if v == item {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}
