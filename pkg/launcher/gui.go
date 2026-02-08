//go:build !nogui

package launcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"image/color"
)

func guiAvailable() bool {
	if os.Getenv("TAIWANFRP_NO_GUI") == "1" {
		return false
	}
	switch runtime.GOOS {
	case "linux":
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	default:
		return true
	}
}

func runGUI() error {
	if !guiAvailable() {
		return errors.New("no gui")
	}
	if err := os.MkdirAll(infoDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", infoDir, err)
	}

	a := app.New()
	if res := loadCJKFontResource(); res != nil {
		a.Settings().SetTheme(&cjkTheme{base: theme.DefaultTheme(), font: res})
	}
	w := a.NewWindow("TaiwanFRP 客戶端")
	w.Resize(fyne.NewSize(900, 640))
	w.SetFixedSize(true)
	if icon := loadAppIcon(); icon != nil {
		w.SetIcon(icon)
	}

	status := widget.NewLabel("請登入")

	info, _ := loadInfoNoPrompt(infoFile)
	if err := configureLauncherDNS(info.DNSServers); err != nil {
		return fmt.Errorf("dns_servers 設定錯誤: %w", err)
	}
	usernameEntry := widget.NewEntry()
	usernameEntry.SetText(info.Username)
	passwordEntry := widget.NewPasswordEntry()
	passwordEntry.SetText(info.Password)

	loginBtn := widget.NewButton("登入", nil)
	loginBox := container.NewVBox(
		buildHeader("TaiwanFRP 客戶端", "登入後即可選擇代理並啟動"),
		widget.NewLabel("使用者名稱"),
		usernameEntry,
		widget.NewLabel("密碼"),
		passwordEntry,
		loginBtn,
		status,
	)

	w.SetContent(container.NewPadded(loginBox))

	var mu sync.Mutex
	var manager *FrpcManager

	showSelection := func(info infoFileData) {
		w.SetFullScreen(false)
		nodes, err := fetchNodes()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		node2iniContent := map[string]string{}
		node2proxies := map[string][]string{}
		nodeByName := map[string]nodeInfo{}
		for _, node := range nodes {
			nodeByName[node.Name] = node
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
			dialog.ShowError(fmt.Errorf("沒有任何節點有可用代理"), w)
			return
		}

		info.Selected = fixSelected(info.Selected, node2proxies, infoFile)

		checked := map[string]map[string]bool{}
		for _, sel := range info.Selected {
			if _, ok := checked[sel.Node]; !ok {
				checked[sel.Node] = map[string]bool{}
			}
			checked[sel.Node][sel.Proxy] = true
		}

		var autoStartTimer *time.Timer
		var autoStartEligible bool
		resetAutoStart := func() {}
		stopAutoStart := func() {}
		var startSelected func(showDialog bool)
		var stopBtn *widget.Button
		var countdownStop chan struct{}
		countdownLabel := widget.NewLabel("")
		countdownLabel.TextStyle = fyne.TextStyle{Bold: true}
		var statsStop chan struct{}
		var statsTicker *time.Ticker
		lastUpdateLabel := widget.NewLabel("最後更新: -")
		getSelected := func() []selection {
			var selected []selection
			for node, m := range checked {
				for proxy, ok := range m {
					if ok {
						selected = append(selected, selection{Node: node, Proxy: proxy})
					}
				}
			}
			return selected
		}

		type proxyCard struct {
			node           string
			proxy          string
			protocol       string
			localPort      string
			localIP        string
			nodeIP         string
			statusLabel    *widget.Label
			addrLabel      *widget.Label
			trafficLabel   *widget.Label
			connsLabel     *widget.Label
			lastStartLabel *widget.Label
			remoteAddr     string
		}

		formatTraffic := func(bytes int64) string {
			if bytes < 1024 {
				return fmt.Sprintf("%d B", bytes)
			}
			kb := float64(bytes) / 1024.0
			if kb < 1024 {
				return fmt.Sprintf("%.2f KB", kb)
			}
			mb := kb / 1024.0
			if mb < 1024 {
				return fmt.Sprintf("%.2f MB", mb)
			}
			gb := mb / 1024.0
			return fmt.Sprintf("%.2f GB", gb)
		}
		setCardText := func(ref *proxyCard, statusText, remote, trafficText, connsText, lastStartText string) {
			localAddr := ref.localPort
			if ref.localIP != "" {
				localAddr = fmt.Sprintf("%s:%s", ref.localIP, ref.localPort)
			}
			ref.statusLabel.SetText(fmt.Sprintf("狀態: %s", statusText))
			ref.addrLabel.SetText(fmt.Sprintf("本地: %s => %s", localAddr, remote))
			ref.trafficLabel.SetText(fmt.Sprintf("今日流量: %s", trafficText))
			ref.connsLabel.SetText(fmt.Sprintf("連線數量: %s", connsText))
			ref.lastStartLabel.SetText(fmt.Sprintf("上次啟動: %s", lastStartText))
			ref.remoteAddr = remote
		}

		parseLocalPort := func(iniContent, proxy string) string {
			if iniContent == "" || proxy == "" {
				return "-"
			}
			inSection := false
			for _, line := range strings.Split(iniContent, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
					continue
				}
				if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
					name := strings.Trim(line, "[]")
					inSection = name == proxy
					continue
				}
				if inSection && strings.HasPrefix(line, "local_port") {
					parts := strings.SplitN(line, "=", 2)
					if len(parts) == 2 {
						return strings.TrimSpace(parts[1])
					}
				}
			}
			return "-"
		}
		parseLocalIP := func(iniContent, proxy string) string {
			if iniContent == "" || proxy == "" {
				return "127.0.0.1"
			}
			inSection := false
			for _, line := range strings.Split(iniContent, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
					continue
				}
				if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
					name := strings.Trim(line, "[]")
					inSection = name == proxy
					continue
				}
				if inSection && strings.HasPrefix(line, "local_ip") {
					parts := strings.SplitN(line, "=", 2)
					if len(parts) == 2 {
						ip := strings.TrimSpace(parts[1])
						if ip != "" {
							return ip
						}
					}
				}
			}
			return "127.0.0.1"
		}

		type tunnelStatus struct {
			Status          string `json:"status"`
			CurConns        int    `json:"cur_conns"`
			LastStartTime   string `json:"last_start_time"`
			TodayTrafficIn  int64  `json:"today_traffic_in"`
			TodayTrafficOut int64  `json:"today_traffic_out"`
			RemotePort      *int   `json:"remote_port"`
		}

		httpClient := &http.Client{Timeout: 5 * time.Second}
		fetchStatus := func(username, password, nodeName, proxyName, protocol string) (*tunnelStatus, error) {
			body := map[string]string{
				"username":   username,
				"password":   password,
				"tunnelName": proxyName,
				"protocol":   protocol,
				"nodeName":   nodeName,
			}
			data, _ := json.Marshal(body)
			req, err := http.NewRequest("POST", serverURL+"/check_tunnel", bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpClient.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("status %d", resp.StatusCode)
			}
			var out tunnelStatus
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return nil, err
			}
			return &out, nil
		}

		list := container.NewVBox()
		proxyChecks := map[string]*widget.Check{}
		for _, node := range nodes {
			proxies := node2proxies[node.Name]
			if len(proxies) == 0 {
				continue
			}
			nodeName := node.Name
			nodeChecks := []*widget.Check{}
			nodeToggle := widget.NewButton("全選此節點", func() {
				all := true
				if _, ok := checked[nodeName]; !ok {
					checked[nodeName] = map[string]bool{}
				}
				for _, p := range proxies {
					if !checked[nodeName][p] {
						all = false
						break
					}
				}
				for i, p := range proxies {
					target := !all
					checked[nodeName][p] = target
					nodeChecks[i].SetChecked(target)
				}
				autoStartEligible = false
				stopAutoStart()
			})
			headerRow := container.NewHBox(
				widget.NewLabelWithStyle(node.Name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				layout.NewSpacer(),
				nodeToggle,
			)
			list.Add(headerRow)
			for _, proxy := range proxies {
				nodeName := node.Name
				proxyName := proxy
				chk := widget.NewCheck(proxyName, func(b bool) {
					if _, ok := checked[nodeName]; !ok {
						checked[nodeName] = map[string]bool{}
					}
					checked[nodeName][proxyName] = b
					autoStartEligible = false
					stopAutoStart()
				})
				if checked[nodeName][proxyName] {
					chk.SetChecked(true)
				}
				nodeChecks = append(nodeChecks, chk)
				proxyChecks[nodeName+"|"+proxyName] = chk
				list.Add(chk)
			}
		}

		selectAllBtn := widget.NewButton("全選", func() {
			all := true
			for nodeName, proxies := range node2proxies {
				for _, p := range proxies {
					if !checked[nodeName][p] {
						all = false
						break
					}
				}
				if !all {
					break
				}
			}
			for _, node := range nodes {
				proxies := node2proxies[node.Name]
				if len(proxies) == 0 {
					continue
				}
				if _, ok := checked[node.Name]; !ok {
					checked[node.Name] = map[string]bool{}
				}
				for _, p := range proxies {
					checked[node.Name][p] = !all
					if chk, ok := proxyChecks[node.Name+"|"+p]; ok {
						chk.SetChecked(!all)
					}
				}
			}
			autoStartEligible = false
			stopAutoStart()
		})

		scroll := container.NewVScroll(list)
		scroll.SetMinSize(fyne.NewSize(600, 400))

		logBox := widget.NewRichText()
		logBox.Wrapping = fyne.TextWrapOff
		logBox.Truncation = fyne.TextTruncateOff
		logScroll := container.NewScroll(logBox)
		logScroll.Direction = container.ScrollBoth
		logScroll.SetMinSize(fyne.NewSize(600, 400))
		logEntry := widget.NewMultiLineEntry()
		logEntry.Wrapping = fyne.TextWrapOff
		logEntry.TextStyle = fyne.TextStyle{Monospace: true}
		logEntry.SetText("")
		logEntryScroll := container.NewScroll(logEntry)
		logEntryScroll.Direction = container.ScrollBoth
		logEntryScroll.SetMinSize(fyne.NewSize(600, 400))
		logPlain := ""
		useSelectable := false
		logContent := container.NewMax(logScroll, logEntryScroll)
		logEntryScroll.Hide()
		cardsGrid := container.NewGridWithColumns(2)
		cardsScroll := container.NewVScroll(cardsGrid)
		cardsScroll.SetMinSize(fyne.NewSize(600, 220))
		var cardRefs []*proxyCard
		var logSegments []widget.RichTextSegment
		const maxLogLines = 2000

		runOnMain := func(fn func()) {
			if runner, ok := fyne.CurrentApp().Driver().(interface{ RunOnMain(func()) }); ok {
				runner.RunOnMain(fn)
				return
			}
			fn()
		}

		appendLog := func(node, line string) {
			runOnMain(func() {
				full := fmt.Sprintf("[%s] %s", node, line)
				logPlain += full + "\n"
				style := logStyleForLine(full)
				logSegments = append(logSegments, &widget.TextSegment{
					Style: style,
					Text:  full + "\n",
				})
				if len(logSegments) > maxLogLines {
					logSegments = logSegments[len(logSegments)-maxLogLines:]
				}
				logBox.Segments = logSegments
				logBox.Refresh()
				logScroll.ScrollToBottom()
				if useSelectable {
					logEntry.SetText(logPlain)
					logEntryScroll.ScrollToBottom()
				}
			})
		}

		buildCards := func(selected []selection) {
			cardsGrid.Objects = nil
			cardRefs = nil
			for _, sel := range selected {
				proxyName := sel.Proxy
				protocol := "tcp"
				apiName := proxyName
				if strings.HasSuffix(proxyName, ",udp") {
					protocol = "udp"
					apiName = strings.TrimSuffix(proxyName, ",udp")
				}
				localPort := "-"
				localIP := "127.0.0.1"
				if ini, ok := node2iniContent[sel.Node]; ok {
					localPort = parseLocalPort(ini, proxyName)
					localIP = parseLocalIP(ini, proxyName)
				}
				nodeIP := ""
				if n, ok := nodeByName[sel.Node]; ok {
					nodeIP = n.IP
				}

				statusLabel := widget.NewLabel("狀態: -")
				addrLabel := widget.NewLabel("本地: -")
				trafficLabel := widget.NewLabel("今日流量: -")
				connsLabel := widget.NewLabel("連線數量: -")
				lastStartLabel := widget.NewLabel("上次啟動: -")
				for _, l := range []*widget.Label{statusLabel, addrLabel, trafficLabel, connsLabel, lastStartLabel} {
					l.Wrapping = fyne.TextWrapWord
				}
				cardRef := &proxyCard{
					localPort:      localPort,
					localIP:        localIP,
					statusLabel:    statusLabel,
					addrLabel:      addrLabel,
					trafficLabel:   trafficLabel,
					connsLabel:     connsLabel,
					lastStartLabel: lastStartLabel,
				}
				copyBtn := widget.NewButton("複製遠端IP", func() {
					if cardRef.remoteAddr == "" || cardRef.remoteAddr == "-" || cardRef.remoteAddr == "N/A" {
						w.Clipboard().SetContent("")
						return
					}
					w.Clipboard().SetContent(cardRef.remoteAddr)
				})
				setCardText(cardRef, "-", "-", "-", "-", "-")

				content := container.NewVBox(statusLabel, addrLabel, trafficLabel, connsLabel, lastStartLabel, copyBtn)
				card := widget.NewCard(proxyName, sel.Node, content)
				cardsGrid.Add(card)
				cardRef.node = sel.Node
				cardRef.proxy = apiName
				cardRef.protocol = protocol
				cardRef.nodeIP = nodeIP
				cardRefs = append(cardRefs, cardRef)
			}
			cardsGrid.Refresh()
		}

		refreshStats := func() {
			refs := append([]*proxyCard(nil), cardRefs...)
			for _, ref := range refs {
				refCopy := ref
				status, err := fetchStatus(info.Username, info.Password, refCopy.node, refCopy.proxy, refCopy.protocol)
				runOnMain(func() {
					if err != nil || status == nil {
						setCardText(refCopy, "取得失敗", "N/A", "N/A", "N/A", "N/A")
						return
					}
					statusText := localizeTunnelStatus(status.Status)
					remoteHost := refCopy.nodeIP
					if remoteHost == "" {
						remoteHost = refCopy.node
					}
					remote := "N/A"
					if status.RemotePort != nil {
						remote = fmt.Sprintf("%s:%d", remoteHost, *status.RemotePort)
					}
					trafficText := fmt.Sprintf("入 %s / 出 %s", formatTraffic(status.TodayTrafficIn), formatTraffic(status.TodayTrafficOut))
					connsText := fmt.Sprintf("%d", status.CurConns)
					last := status.LastStartTime
					if last == "" {
						last = "N/A"
					}
					setCardText(refCopy, statusText, remote, trafficText, connsText, last)
				})
			}
			runOnMain(func() {
				lastUpdateLabel.SetText("最後更新: " + time.Now().Format("15:04:05"))
			})
		}

		startStats := func() {
			if statsTicker != nil {
				return
			}
			stopCh := make(chan struct{})
			ticker := time.NewTicker(10 * time.Second)
			statsStop = stopCh
			statsTicker = ticker
			refreshStats()
			go func(stopCh chan struct{}, ticker *time.Ticker) {
				for {
					select {
					case <-ticker.C:
						refreshStats()
					case <-stopCh:
						return
					}
				}
			}(stopCh, ticker)
		}

		stopStats := func() {
			ticker := statsTicker
			stopCh := statsStop
			statsTicker = nil
			statsStop = nil
			if ticker != nil {
				ticker.Stop()
			}
			if stopCh != nil {
				close(stopCh)
			}
		}

		var selectionPage fyne.CanvasObject
		var logPage fyne.CanvasObject

		stopAll := func() {
			mu.Lock()
			defer mu.Unlock()
			if manager != nil {
				manager.Stop()
				manager.Wait()
				manager = nil
				appendLog("系統", "所有 frpc 已停止")
			}
			stopStats()
			cardRefs = nil
			cardsGrid.Objects = nil
			cardsGrid.Refresh()
			lastUpdateLabel.SetText("最後更新: -")
		}

		logout := func() {
			_ = os.RemoveAll(infoDir)
			stopAutoStart()
			stopAll()
			runLoginScreen(w, status, usernameEntry, passwordEntry, loginBtn)
		}
		copyLogs := func() {
			w.Clipboard().SetContent(logPlain)
		}

		autoStartDelay := 10 * time.Second
		stopAutoStart = func() {
			if autoStartTimer != nil {
				autoStartTimer.Stop()
			}
			if countdownStop != nil {
				close(countdownStop)
				countdownStop = nil
			}
			runOnMain(func() { countdownLabel.SetText("") })
		}
		resetAutoStart = func() {
			if !autoStartEligible || len(getSelected()) == 0 {
				if autoStartTimer != nil {
					autoStartTimer.Stop()
				}
				if countdownStop != nil {
					close(countdownStop)
					countdownStop = nil
				}
				runOnMain(func() { countdownLabel.SetText("") })
				return
			}
			if autoStartTimer == nil {
				autoStartTimer = time.AfterFunc(autoStartDelay, func() {
					runOnMain(func() {
						startSelected(false)
					})
				})
			}
			autoStartTimer.Reset(autoStartDelay)
			if countdownStop == nil {
				countdownStop = make(chan struct{})
				go func(ch chan struct{}) {
					remaining := int(autoStartDelay.Seconds())
					for {
						runOnMain(func() {
							countdownLabel.SetText(fmt.Sprintf("將在 %d 秒後自動啟動", remaining))
						})
						if remaining <= 0 {
							return
						}
						select {
						case <-time.After(time.Second):
							remaining--
						case <-ch:
							return
						}
					}
				}(countdownStop)
			}
		}

		startSelected = func(showDialog bool) {
			stopAutoStart()
			mu.Lock()
			if manager != nil {
				mu.Unlock()
				if showDialog {
					dialog.ShowInformation("提示", "目前已在啟動中，請先停止再重新啟動。", w)
				}
				return
			}
			mu.Unlock()
			selected := getSelected()
			if len(selected) == 0 {
				if showDialog {
					dialog.ShowInformation("提示", "請至少選擇一個代理", w)
				}
				return
			}
			buildCards(selected)
			info.Selected = selected
			if err := os.MkdirAll(infoDir, 0755); err != nil {
				dialog.ShowError(err, w)
				return
			}
			if err := saveInfo(infoFile, info); err != nil {
				dialog.ShowError(err, w)
				return
			}
			nodeSelected := map[string][]string{}
			for _, sel := range info.Selected {
				nodeSelected[sel.Node] = append(nodeSelected[sel.Node], sel.Proxy)
			}
			frpcPath, err := findFrpcBinary()
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			m, err := startFrpcProcesses(frpcPath, nodes, node2iniContent, nodeSelected, appendLog)
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			mu.Lock()
			manager = m
			mu.Unlock()
			appendLog("系統", "所有 frpc 已啟動")
			startStats()
			if stopBtn != nil {
				stopBtn.SetText("停止")
			}
			if logPage != nil {
				w.SetContent(logPage)
			}
		}

		startBtn := widget.NewButton("啟動", func() {
			startSelected(true)
		})

		backBtn := widget.NewButton("返回選擇", func() {
			stopAll()
			if selectionPage != nil {
				w.SetContent(selectionPage)
			}
			autoStartEligible = false
			stopAutoStart()
		})

		stopBtn = widget.NewButton("停止", func() {
			mu.Lock()
			running := manager != nil
			mu.Unlock()
			if running {
				stopAll()
				stopBtn.SetText("啟動")
				return
			}
			startSelected(true)
			stopBtn.SetText("停止")
		})
		logoutBtn := widget.NewButton("登出", logout)

		headerSubtitle := countdownLabel
		actions := container.NewHBox(startBtn, selectAllBtn, layout.NewSpacer(), widget.NewButton("登出", logout))
		selectionPage = container.NewBorder(
			container.NewVBox(buildHeaderWithSubtitle("選擇代理", headerSubtitle), actions),
			nil,
			nil,
			nil,
			scroll,
		)
		selectableToggle := widget.NewCheck("可選取模式", func(b bool) {
			useSelectable = b
			if b {
				logScroll.Hide()
				logEntryScroll.Show()
				logEntry.SetText(logPlain)
				logEntryScroll.ScrollToBottom()
			} else {
				logEntryScroll.Hide()
				logScroll.Show()
				logScroll.ScrollToBottom()
			}
			logContent.Refresh()
		})
		copyBtn := widget.NewButton("複製日誌", copyLogs)
		logActions := container.NewHBox(backBtn, stopBtn, layout.NewSpacer(), selectableToggle, copyBtn, logoutBtn)
		cardsTitle := widget.NewLabelWithStyle("啟用中的代理", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		cardsPanel := container.NewBorder(container.NewHBox(cardsTitle, layout.NewSpacer(), lastUpdateLabel), nil, nil, nil, cardsScroll)
		logTitle := widget.NewLabelWithStyle("日誌輸出", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		logPanel := container.NewBorder(logTitle, nil, nil, nil, logContent)
		split := container.NewVSplit(cardsPanel, logPanel)
		split.SetOffset(0.5)
		logPage = container.NewBorder(
			container.NewVBox(buildHeader("啟動監控", ""), logActions),
			nil,
			nil,
			nil,
			split,
		)
		w.SetContent(container.NewPadded(selectionPage))
		autoStartEligible = len(info.Selected) > 0
		resetAutoStart()
	}

	loginBtn.OnTapped = func() {
		username := strings.TrimSpace(usernameEntry.Text)
		password := strings.TrimSpace(passwordEntry.Text)
		if username == "" || password == "" {
			status.SetText("請輸入帳號密碼")
			return
		}
		info.Username = username
		info.Password = password
		if err := configureLauncherDNS(info.DNSServers); err != nil {
			status.SetText("dns_servers 設定錯誤")
			dialog.ShowError(err, w)
			return
		}
		if err := verifyLogin(info); err != nil {
			status.SetText("登入失敗")
			dialog.ShowError(err, w)
			return
		}
		if err := os.MkdirAll(infoDir, 0755); err != nil {
			status.SetText("建立登入資料夾失敗")
			dialog.ShowError(err, w)
			return
		}
		_ = saveInfo(infoFile, info)
		status.SetText(fmt.Sprintf("登入成功，當前使用者 %s", info.Username))
		showSelection(info)
	}

	if info.Username != "" && info.Password != "" {
		if err := verifyLogin(info); err == nil {
			status.SetText(fmt.Sprintf("登入成功，當前使用者 %s", info.Username))
			showSelection(info)
		}
	}

	w.ShowAndRun()
	return nil
}

func logStyleForLine(line string) widget.RichTextStyle {
	style := widget.RichTextStyle{
		Inline: true,
		TextStyle: fyne.TextStyle{
			Monospace: true,
		},
	}
	if strings.Contains(line, "[E]") || strings.Contains(line, "錯誤") {
		style.ColorName = theme.ColorNameError
		return style
	}
	if strings.Contains(line, "[W]") || strings.Contains(line, "警告") {
		style.ColorName = theme.ColorNameWarning
		return style
	}
	if strings.Contains(line, "[I]") || strings.Contains(line, "啟動成功") {
		style.ColorName = theme.ColorNameSuccess
		return style
	}
	style.ColorName = theme.ColorNameForeground
	return style
}

func localizeTunnelStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "online":
		return "在線"
	case "offline":
		return "離線"
	default:
		if s == "" {
			return "-"
		}
		return s
	}
}

func loadInfoNoPrompt(path string) (infoFileData, error) {
	if _, err := os.Stat(path); err == nil {
		return loadInfo(path)
	}
	return infoFileData{}, nil
}

func buildHeader(title, subtitle string) fyne.CanvasObject {
	titleLabel := widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	subtitleLabel := widget.NewLabel(subtitle)
	textBox := container.NewVBox(titleLabel, subtitleLabel)

	iconRes := loadAppIcon()
	if iconRes != nil {
		icon := widget.NewIcon(iconRes)
		iconBox := container.NewGridWrap(fyne.NewSize(28, 28), icon)
		return container.NewVBox(container.NewHBox(iconBox, textBox), widget.NewSeparator())
	}
	return container.NewVBox(textBox, widget.NewSeparator())
}

func buildHeaderWithSubtitle(title string, subtitle *widget.Label) fyne.CanvasObject {
	titleLabel := widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	textBox := container.NewVBox(titleLabel, subtitle)

	iconRes := loadAppIcon()
	if iconRes != nil {
		icon := widget.NewIcon(iconRes)
		iconBox := container.NewGridWrap(fyne.NewSize(28, 28), icon)
		return container.NewVBox(container.NewHBox(iconBox, textBox), widget.NewSeparator())
	}
	return container.NewVBox(textBox, widget.NewSeparator())
}

func loadAppIcon() fyne.Resource {
	if p := os.Getenv("TAIWANFRP_ICON"); p != "" {
		if res := resourceFromFile(p); res != nil {
			return res
		}
	}
	exeDir := ""
	if exe, err := os.Executable(); err == nil && exe != "" {
		exeDir = filepath.Dir(exe)
	}
	candidates := []string{
		"taiwanfrp.ico",
		filepath.Join("client", "taiwanfrp.ico"),
		filepath.Join("assets", "taiwanfrp.ico"),
		filepath.Join(exeDir, "taiwanfrp.ico"),
		filepath.Join(exeDir, "client", "taiwanfrp.ico"),
		filepath.Join(exeDir, "assets", "taiwanfrp.ico"),
	}
	for _, p := range candidates {
		if res := resourceFromFile(p); res != nil {
			return res
		}
	}
	return nil
}

func runLoginScreen(w fyne.Window, status *widget.Label, user *widget.Entry, pass *widget.Entry, loginBtn *widget.Button) {
	user.SetText("")
	pass.SetText("")
	status.SetText("請登入")
	loginBox := container.NewVBox(
		buildHeader("TaiwanFRP 客戶端", "登入後即可選擇代理並啟動"),
		widget.NewLabel("使用者名稱"),
		user,
		widget.NewLabel("密碼"),
		pass,
		loginBtn,
		status,
	)
	w.SetContent(container.NewPadded(loginBox))
}

type cjkTheme struct {
	base fyne.Theme
	font fyne.Resource
}

func (t *cjkTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	return t.base.Color(name, variant)
}
func (t *cjkTheme) Font(style fyne.TextStyle) fyne.Resource {
	if t.font != nil {
		return t.font
	}
	return t.base.Font(style)
}
func (t *cjkTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return t.base.Icon(name)
}
func (t *cjkTheme) Size(name fyne.ThemeSizeName) float32 {
	return t.base.Size(name)
}

func loadCJKFontResource() fyne.Resource {
	if p := os.Getenv("TAIWANFRP_FONT"); p != "" {
		if res := resourceFromFile(p); res != nil {
			return res
		}
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
			"/System/Library/Fonts/Supplemental/AppleGothic.ttf",
			"/System/Library/Fonts/Supplemental/Arial.ttf",
		}
	case "windows":
		candidates = []string{
			`C:\Windows\Fonts\msjh.ttc`,
			`C:\Windows\Fonts\msyh.ttc`,
		}
	default:
		candidates = []string{
			"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
		}
	}
	for _, p := range candidates {
		if res := resourceFromFile(p); res != nil {
			return res
		}
	}
	return nil
}

func resourceFromFile(path string) fyne.Resource {
	if path == "" {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".ttc" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	name := filepath.Base(path)
	return fyne.NewStaticResource(name, data)
}
