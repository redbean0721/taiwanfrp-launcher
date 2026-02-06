//go:build !nogui

package launcher

import (
	"errors"
	"fmt"
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

		list := container.NewVBox()
		for _, node := range nodes {
			proxies := node2proxies[node.Name]
			if len(proxies) == 0 {
				continue
			}
			list.Add(widget.NewLabelWithStyle(node.Name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
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
				list.Add(chk)
			}
		}

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
		}

		logout := func() {
			_ = os.Remove(infoFile)
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
		actions := container.NewHBox(startBtn, layout.NewSpacer(), widget.NewButton("登出", logout))
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
		logPage = container.NewBorder(
			container.NewVBox(buildHeader("日誌輸出", ""), logActions),
			nil,
			nil,
			nil,
			logContent,
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
