#!/bin/bash

INFO_DIR="lib"
INFO_FILE="$INFO_DIR/info.json"
FRPC_INI_FILE="$INFO_DIR/frpc.ini"
FRPCRUN_FILE="$INFO_DIR/frpcrun"
JQ_FILE="$INFO_DIR/jq"
SERVER_URL="https://taiwanfrp.ddns.net"

# Create lib directory if it doesn't exist
if [ ! -d "$INFO_DIR" ]; then
    mkdir -p "$INFO_DIR"
fi

# Check if jq is installed
if ! command -v jq &> /dev/null; then
    echo "jq 未安裝，正在安裝..."
    if command -v apt &> /dev/null; then
        sudo apt update && sudo apt install -y jq
    elif command -v yum &> /dev/null; then
        sudo yum install -y jq
    else
        echo "無法安裝 jq，請手動安裝。"
        exit 1
    fi
fi

login_and_setup() {
    username=$(jq -r '.username' "$INFO_FILE")
    password=$(jq -r '.password' "$INFO_FILE")

    # Verify server connection
    response=$(curl -s -w "%{http_code}" -o /dev/null -X POST -H "Content-Type: application/json" -d "{\"username\":\"$username\",\"password\":\"$password\"}" "$SERVER_URL/login")
    if [ "$response" -eq 200 ]; then
        echo "登入成功"

        # Fetch nodes from server
        nodes=$(curl -s "$SERVER_URL/nodes.json" | jq -r '.nodes')

        # Check if node is already saved in info.json
        saved_node=$(jq -r '.node' "$INFO_FILE")
        if [ "$saved_node" == "null" ]; then
            # List nodes for user selection
            echo "可用節點:"
            echo "$nodes" | jq -r '.[] | "\(.name)"' | nl -w 2 -s ') '
            while true; do
                read -p "選擇節點（輸入節點代碼）: " node_index
                if [[ -z "$node_index" ]] || ! [[ "$node_index" =~ ^[0-9]+$ ]] || [ "$node_index" -lt 1 ] || [ "$node_index" -gt "$(echo "$nodes" | jq -r '. | length')" ]; then
                    echo "無效的節點代碼，請重新輸入。"
                else
                    selected_node=$(echo "$nodes" | jq -r ".[$((node_index-1))]")
                    break
                fi
            done

            # Save selected node to info.json
            jq --argjson node "$selected_node" '.node = $node' "$INFO_FILE" > "$INFO_FILE.tmp" && mv "$INFO_FILE.tmp" "$INFO_FILE"
        else
            selected_node="$saved_node"
        fi

        # Remove existing frpc.ini and frpcrun
        [ -f "$FRPC_INI_FILE" ] && rm "$FRPC_INI_FILE"
        [ -f "$FRPCRUN_FILE" ] && rm "$FRPCRUN_FILE"

        # Read frpc.ini content from selected node with basic authentication
        frpc_ini_content=$(curl -s -u "$username:$password" "$SERVER_URL/$(echo "$selected_node" | jq -r '.frpcIniFolder')/$username/frpc.ini")

        # Check if tunnels are already saved in info.json
        saved_tunnels=$(jq -r '.tunnels' "$INFO_FILE")
        if [ "$saved_tunnels" == "null" ]; then
            # Parse tunnels from frpc.ini content and let user select
            tunnels=$(echo "$frpc_ini_content" | grep -oP '^\[\K[^\]]+' | grep -v '^common$')
            tunnel_list=($tunnels)
            selected_tunnels=()
            while true; do
                echo "當前已選擇隧道: ${selected_tunnels[*]}"
                echo "可用隧道:"
                for i in "${!tunnel_list[@]}"; do
                    echo "$((i+1))) ${tunnel_list[$i]}"
                done
                echo "(輸入 'stop' 表示已經選完所有隧道, 'all' 表示選擇所有隧道, 新手建議直接輸入all):"
                read -p "選擇隧道（隧道代碼，就是隧道名稱前面的數字，輸入已選擇的隧道代碼取消選擇。）: " input
                if [ "$input" == "stop" ] || [ "$input" == "all" ]; then
                    if [ "$input" == "all" ]; then
                        selected_tunnels=("${tunnel_list[@]}")
                    fi
                    jq --argjson tunnels "$(printf '%s\n' "${selected_tunnels[@]}" | jq -R . | jq -s .)" '.tunnels = $tunnels' "$INFO_FILE" > "$INFO_FILE.tmp" && mv "$INFO_FILE.tmp" "$INFO_FILE"
                    break
                elif [[ "$input" =~ ^[0-9]+$ ]] && [ "$input" -ge 1 ] && [ "$input" -le "${#tunnel_list[@]}" ]; then
                    tunnel="${tunnel_list[$((input-1))]}"
                    if [[ " ${selected_tunnels[@]} " =~ " ${tunnel} " ]]; then
                        selected_tunnels=($(printf "%s\n" "${selected_tunnels[@]}" | grep -v "^${tunnel}$"))
                        echo "取消選擇隧道: $tunnel"
                    else
                        selected_tunnels+=("$tunnel")
                        echo "已選擇隧道: $tunnel"
                    fi
                else
                    echo "無效的輸入，請重新輸入。"
                fi
                echo "-----分隔線-----"
            done
        else
            selected_tunnels=()
            tunnels=$(echo "$frpc_ini_content" | grep -oP '^\[\K[^\]]+' | grep -v '^common$')
            tunnel_list=($tunnels)
            for tunnel in $(echo "$saved_tunnels" | jq -r '.[]'); do
                if [[ " ${tunnel_list[@]} " =~ " $tunnel " ]]; then
                    selected_tunnels+=("$tunnel")
                else
                    echo "隧道 $tunnel 不存在，已自動移除。"
                fi
            done
            # Update info.json with valid tunnels
            jq --argjson tunnels "$(printf '%s\n' "${selected_tunnels[@]}" | jq -R . | jq -s .)" '.tunnels = $tunnels' "$INFO_FILE" > "$INFO_FILE.tmp" && mv "$INFO_FILE.tmp" "$INFO_FILE"
        fi

        # Filter frpc.ini content based on selected tunnels
        include=false
        filtered_content=$(echo "$frpc_ini_content" | while IFS= read -r line; do
            if [[ "$line" =~ ^\[ ]]; then
                section=$(echo "$line" | sed 's/^\[\(.*\)\]$/\1/')
                if [ "$section" == "common" ] || [[ " ${selected_tunnels[@]} " =~ " $section " ]]; then
                    include=true
                else
                    include=false
                fi
            fi
            $include && echo "$line"
        done)
        echo "$filtered_content" > "$FRPC_INI_FILE"

        # Download frpcrun
        curl -L -o "$FRPCRUN_FILE" "$SERVER_URL/linux/frpcrun"
        chmod +x "$FRPCRUN_FILE"

        # Run frpcrun directly
        cd "$INFO_DIR" && ./frpcrun -c frpc.ini
    elif [ "$response" -eq 400 ]; then
        echo "登入失敗，無效的帳號密碼。"
        rm "$INFO_FILE"
        read -p "Press any key to continue..."
    else
        echo "登入失敗，檢查網路連接。"
        read -p "Press any key to continue..."
    fi
}

if [ ! -f "$INFO_FILE" ]; then
    echo "請輸入使用者名稱和密碼。"
    read -p "使用者名稱: " username
    read -sp "密碼(不會顯示): " password
    echo

    jq -n --arg username "$username" --arg password "$password" '{username: $username, password: $password}' > "$INFO_FILE"

    echo "已經保存登入資料，如要登出請刪除info.json。"
    if ! login_and_setup; then
        if [ "$response" -eq 400 ]; then
            echo "登入失敗，無效的帳號密碼。"
            rm "$INFO_FILE"
        else
            echo "ERROR: $response"
            echo "登入失敗，檢查網路連接。"
        fi
        read -p "Press any key to continue..."
    fi
else
    echo "自動登入中...。"
    if ! login_and_setup; then
        if [ "$response" -eq 400 ]; then
            echo "登入失敗，無效的帳號密碼。"
            rm "$INFO_FILE"
        else
            echo "ERROR: $response"
            echo "登入失敗，檢查網路連接。"
        fi
        read -p "Press any key to continue..."
    fi
fi