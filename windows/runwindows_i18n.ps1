function Select-Language {
    Write-Host "請選擇語言 (Please select a language):"
    Write-Host "1) 繁體中文"
    Write-Host "2) English"
    $choice = Read-Host "請輸入您的選擇 (Please enter your choice)"
    switch ($choice) {
        "1" { $script:language = "zh-TW" }
        "2" { $script:language = "en-US" }
        default {
            Write-Host "無效的選擇，預設為英文。(Invalid selection, defaulting to English.)"
            $script:language = "en-US"
        }
    }
}

$messages = @{
    "zh-TW" = @{
        jq_not_installed = "jq 未安裝，正在安裝..."
        login_successful = "登入成功"
        login_failed_credentials = "登入失敗，無效的帳號密碼。"
        login_failed_network = "登入失敗，請檢查網路連接。"
        available_nodes = "可用節點:"
        select_node = "選擇節點（輸入節點代碼）"
        current_tunnels = "當前已選擇隧道: "
        available_tunnels = "可用隧道:"
        tunnel_selection_prompt = "(輸入 'stop' 表示已經選完所有隧道, 'all' 表示選擇所有隧道, 新手建議直接輸入all):"
        tunnel_selection_input = "選擇隧道（隧道代碼，就是隧道名稱前面的數字，輸入已選擇的隧道代碼取消選擇。）"
        stop_command = "stop"
        all_command = "all"
        invalid_input = "無效的輸入，請重新輸入。"
        separator = "-----分隔線-----"
        tunnel_deselected = "取消選擇隧道: "
        tunnel_selected = "已選擇隧道: "
        tunnel_not_exist = "隧道 {0} 不存在，已自動移除。"
        enter_credentials = "請輸入使用者名稱和密碼。"
        username_prompt = "使用者名稱"
        password_prompt = "密碼"
        credentials_saved = "已經保存登入資料，如要登出請刪除 info.json。"
        auto_login = "自動登入中...。"
        error_prefix = "錯誤: "
    }
    "en-US" = @{
        jq_not_installed = "jq is not installed, installing now..."
        login_successful = "Login successful."
        login_failed_credentials = "Login failed, invalid username or password."
        login_failed_network = "Login failed, please check your network connection."
        available_nodes = "Available nodes:"
        select_node = "Select a node (enter the node number)"
        current_tunnels = "Currently selected tunnels: "
        available_tunnels = "Available tunnels:"
        tunnel_selection_prompt = "(Enter 'stop' when you have finished selecting tunnels, 'all' to select all tunnels. Beginners are advised to enter 'all'):"
        tunnel_selection_input = "Select a tunnel (enter the number, enter a selected number again to deselect)."
        stop_command = "stop"
        all_command = "all"
        invalid_input = "Invalid input, please try again."
        separator = "-----Separator-----"
        tunnel_deselected = "Deselected tunnel: "
        tunnel_selected = "Selected tunnel: "
        tunnel_not_exist = "Tunnel {0} does not exist and has been automatically removed."
        enter_credentials = "Please enter your username and password."
        username_prompt = "Username"
        password_prompt = "Password"
        credentials_saved = "Login information has been saved. To log out, please delete the info.json file."
        auto_login = "Attempting auto-login..."
        error_prefix = "ERROR: "
    }
}

# Select language first
Select-Language
$lang = $messages[$script:language]

$INFO_DIR = "lib"
$INFO_FILE = "$INFO_DIR/info.json"
$FRPC_INI_FILE = "$INFO_DIR/frpc.ini"
$FRPCRUN_FILE = "$INFO_DIR/frpcrun.exe"
$JQ_FILE = "$INFO_DIR/jq.exe"
$SERVER_URL = "https://taiwanfrp.ddns.net"

# Create lib directory if it doesn't exist
if (-not (Test-Path -Path $INFO_DIR)) {
    New-Item -ItemType Directory -Path $INFO_DIR | Out-Null
}

# Check if jq is installed
if (-not (Test-Path -Path $JQ_FILE)) {
    Write-Host $lang.jq_not_installed
    Invoke-WebRequest -Uri "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-win64.exe" -OutFile $JQ_FILE
}

function LoginAndSetup {
    $username = (Get-Content $INFO_FILE | ConvertFrom-Json).username
    $password = (Get-Content $INFO_FILE | ConvertFrom-Json).password

    # Verify server connection
    $response = Invoke-WebRequest -Uri "$SERVER_URL/login" -Method Post -ContentType "application/json" -Body (@{username=$username; password=$password} | ConvertTo-Json) -UseBasicParsing
    if ($response.StatusCode -eq 200) {
        Write-Host $lang.login_successful

        # Fetch nodes from server
        $nodes_response = Invoke-WebRequest -Uri "$SERVER_URL/nodes.json" -UseBasicParsing
        $nodes = ($nodes_response.Content | ConvertFrom-Json).nodes

        # Check if node is already saved in info.json
        $saved_node = (Get-Content $INFO_FILE | ConvertFrom-Json).node
        if (-not $saved_node) {
            # List nodes for user selection
            Write-Host $lang.available_nodes
            for ($i = 0; $i -lt $nodes.Count; $i++) {
                Write-Host "$($i+1)) $($nodes[$i].name)"
            }
            $node_index = Read-Host $lang.select_node
            $selected_node = $nodes[[int]$node_index - 1]

            # Save selected node to info.json
            $json = Get-Content $INFO_FILE | ConvertFrom-Json
            $json.node = $selected_node
            $json | ConvertTo-Json | Set-Content $INFO_FILE
        } else {
            $selected_node = $saved_node
        }

        # Remove existing frpc.ini and frpcrun
        if (Test-Path -Path $FRPC_INI_FILE) {
            Remove-Item -Path $FRPC_INI_FILE
        }
        if (Test-Path -Path $FRPCRUN_FILE) {
            Remove-Item -Path $FRPCRUN_FILE
        }

        # Read frpc.ini content from selected node with basic authentication
        $frpc_ini_content = Invoke-WebRequest -Uri "$SERVER_URL/$($selected_node.frpcIniFolder)/$username/frpc.ini" -Headers @{Authorization=("Basic " + [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes("$username:$password")))} -UseBasicParsing | Select-Object -ExpandProperty Content

        # Check if tunnels are already saved in info.json
        $saved_tunnels = (Get-Content $INFO_FILE | ConvertFrom-Json).tunnels
        if (-not $saved_tunnels) {
            # Parse tunnels from frpc.ini content and let user select
            $tunnels = $frpc_ini_content -split "`n" | Where-Object { $_ -match '^\[' -and $_ -notmatch '^\[common\]' } | ForEach-Object { $_ -replace '^\[|\]$' }
            $tunnel_list = @($tunnels)
            $selected_tunnels = @()
            while ($true) {
                Write-Host ($lang.current_tunnels + ($selected_tunnels -join ' '))
                Write-Host $lang.available_tunnels
                for ($i = 0; $i -lt $tunnel_list.Count; $i++) {
                    Write-Host "$($i+1)) $($tunnel_list[$i])"
                }
                Write-Host $lang.tunnel_selection_prompt
                $input = Read-Host $lang.tunnel_selection_input
                if ($input.ToLower() -eq $lang.stop_command -or $input.ToLower() -eq $lang.all_command) {
                    if ($input.ToLower() -eq $lang.all_command) {
                        $selected_tunnels = $tunnel_list
                    }
                    # Ensure the tunnels property exists before setting it
                    $json = Get-Content $INFO_FILE | ConvertFrom-Json
                    if (-not $json.PSObject.Properties["tunnels"]) {
                        $json | Add-Member -MemberType NoteProperty -Name "tunnels" -Value @()
                    }
                    $json.tunnels = $selected_tunnels
                    $json | ConvertTo-Json | Set-Content $INFO_FILE
                    break
                } elseif ($input -match '^\d+$' -and [int]$input -ge 1 -and [int]$input -le $tunnel_list.Count) {
                    $tunnel = $tunnel_list[[int]$input - 1]
                    if ($selected_tunnels -contains $tunnel) {
                        $selected_tunnels = $selected_tunnels | Where-Object { $_ -ne $tunnel }
                        Write-Host ($lang.tunnel_deselected + $tunnel)
                    } else {
                        $selected_tunnels += $tunnel
                        Write-Host ($lang.tunnel_selected + $tunnel)
                    }
                    $selected_tunnels = $selected_tunnels -join ' ' -split ' '
                    Write-Host $lang.separator
                } else {
                    Write-Host $lang.invalid_input
                    Write-Host $lang.separator
                }
            }

        } else {
            $selected_tunnels = @()
            $tunnels = $frpc_ini_content -split "`n" | Where-Object { $_ -match '^\[' -and $_ -notmatch '^\[common\]' } | ForEach-Object { $_ -replace '^\[|\]$' }
            $tunnel_list = @($tunnels)
            foreach ($tunnel in $saved_tunnels) {
                if ($tunnel_list -contains $tunnel) {
                    $selected_tunnels += $tunnel
                } else {
                    Write-Host ($lang.tunnel_not_exist -f $tunnel)
                }
            }
            # Update info.json with valid tunnels
            $json = Get-Content $INFO_FILE | ConvertFrom-Json
            $json.tunnels = $selected_tunnels
            $json | ConvertTo-Json | Set-Content $INFO_FILE
        }

        # Filter frpc.ini content based on selected tunnels
        $filtered_content = $frpc_ini_content -split "`n" | ForEach-Object {
            if ($_ -match '^\[') {
                $section = $_ -replace '^\[|\]$'
                if ($section -eq 'common' -or $selected_tunnels -contains $section) {
                    $include = $true
                } else {
                    $include = $false
                }
            }
            if ($include) { $_ }
        }
        $filtered_content | Out-File -FilePath $FRPC_INI_FILE -Encoding utf8

        # Download frpcrun
        Invoke-WebRequest -Uri "$SERVER_URL/windows/frpcrun.exe" -OutFile $FRPCRUN_FILE

        # Set executable permissions for frpcrun
        icacls $FRPCRUN_FILE /grant Everyone:F

        # Run frpcrun in a new window and change to lib directory
        Start-Process -FilePath "powershell" -ArgumentList "-NoExit", "-Command", "cd $INFO_DIR; .\frpcrun.exe -c frpc.ini" -WindowStyle Normal
    } elseif ($response.StatusCode -eq 400) {
        Write-Host $lang.login_failed_credentials
        Remove-Item -Path $INFO_FILE
        pause
    } else {
        Write-Host $lang.login_failed_network
        pause
    }
}

if (-not (Test-Path -Path $INFO_FILE)) {
    Write-Host $lang.enter_credentials
    $username = Read-Host $lang.username_prompt
    $password = Read-Host $lang.password_prompt

    $json = @{
        username = $username
        password = $password
    } | ConvertTo-Json

    $json | Set-Content $INFO_FILE

    Write-Host $lang.credentials_saved
    try {
        LoginAndSetup
    } catch {
        if ($_.Exception.Message -match "400") {
            Write-Host $lang.login_failed_credentials
            Remove-Item -Path $INFO_FILE
        } else {
            Write-Host ($lang.error_prefix + $_.Exception.Message)
            Write-Host $lang.login_failed_network
        }
        pause
    }
} else {
    Write-Host $lang.auto_login
    try {
        LoginAndSetup
    } catch {
        if ($_.Exception.Message -match "400") {
            Write-Host $lang.login_failed_credentials
            Remove-Item -Path $INFO_FILE
        } else {
            Write-Host ($lang.error_prefix + $_.Exception.Message)
            Write-Host $lang.login_failed_network
        }
        pause
    }
}