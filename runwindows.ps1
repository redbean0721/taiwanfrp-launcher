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
    Write-Host "jq 未安裝，正在安裝..."
    Invoke-WebRequest -Uri "https://github.com/stedolan/jq/releases/download/jq-1.6/jq-win64.exe" -OutFile $JQ_FILE
}

function LoginAndSetup {
    $username = (Get-Content $INFO_FILE | ConvertFrom-Json).username
    $password = (Get-Content $INFO_FILE | ConvertFrom-Json).password

    # Verify server connection
    $response = Invoke-WebRequest -Uri "$SERVER_URL/login" -Method Post -ContentType "application/json" -Body (@{username=$username; password=$password} | ConvertTo-Json) -UseBasicParsing
    if ($response.StatusCode -eq 200) {
        Write-Host "登入成功"

        # Remove existing frpc.ini and frpcrun
        if (Test-Path -Path $FRPC_INI_FILE) {
            Remove-Item -Path $FRPC_INI_FILE
        }
        if (Test-Path -Path $FRPCRUN_FILE) {
            Remove-Item -Path $FRPCRUN_FILE
        }

        # Read frpc.ini content from server
        $frpc_ini_content = Invoke-WebRequest -Uri "$SERVER_URL/frpcini/$username/frpc.ini" -UseBasicParsing | Select-Object -ExpandProperty Content

        # Check if tunnels are already saved in info.json
        $saved_tunnels = (Get-Content $INFO_FILE | ConvertFrom-Json).tunnels
        if (-not $saved_tunnels) {
            # Parse tunnels from frpc.ini content and let user select
            $tunnels = $frpc_ini_content -split "`n" | Where-Object { $_ -match '^\[' -and $_ -notmatch '^\[common\]' } | ForEach-Object { $_ -replace '^\[|\]$' }
            $tunnel_list = @($tunnels)
            $selected_tunnels = @()
            while ($true) {
                Write-Host "當前已選擇隧道: $($selected_tunnels -join ' ')"
                Write-Host "可用隧道:"
                for ($i = 0; $i -lt $tunnel_list.Count; $i++) {
                    Write-Host "$($i+1)) $($tunnel_list[$i])"
                }
                Write-Host "(輸入 'stop' 表示已經選完所有隧道, 'all' 表示選擇所有隧道, 新手建議直接輸入all):"
                $input = Read-Host "選擇隧道（隧道代碼，就是隧道名稱前面的數字，輸入已選擇的隧道代碼取消選擇。）"
                if ($input -eq "stop" -or $input -eq "all") {
                    if ($input -eq "all") {
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
                        Write-Host "取消選擇隧道: $tunnel"
                    } else {
                        $selected_tunnels += $tunnel
                        Write-Host "已選擇隧道: $tunnel"
                    }
                    $selected_tunnels = $selected_tunnels -join ' ' -split ' '
                    Write-Host "-----分隔線-----"
                } else {
                    Write-Host "無效的輸入，請重新輸入。"
                    Write-Host "-----分隔線-----"
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
                    Write-Host "隧道 $tunnel 不存在，已自動移除。"
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
        Write-Host "登入失敗，無效的帳號密碼。"
        Remove-Item -Path $INFO_FILE
    } else {
        Write-Host "登入失敗，檢查網路連接。"
    }
}

if (-not (Test-Path -Path $INFO_FILE)) {
    Write-Host "請輸入使用者名稱和密碼。"
    $username = Read-Host "使用者名稱"
    $password = Read-Host "密碼"

    $json = @{
        username = $username
        password = $password
    } | ConvertTo-Json

    $json | Set-Content $INFO_FILE

    Write-Host "已經保存登入資料，如要登出請刪除info.json。"
    LoginAndSetup
} else {
    Write-Host "自動登入中...。"
    LoginAndSetup
}
