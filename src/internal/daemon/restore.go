package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// RestoreStateEnv 是 handoff 时旧 daemon 通过环境变量传给新 daemon 的
// bridge 恢复配置（base64 JSON）。平台无关，非 Unix 平台不使用。
const RestoreStateEnv = "ATB_RESTORE"

// BridgeRestore 描述一台 bridge 在 daemon 优雅重启后原样恢复所需的全部状态：
// 旧 daemon 在 handoff 前快照，随 exec 传给新 daemon，新 daemon 据此重建
// 后端配置并直接接管已传递的 listener（不重新绑定端口、不重新查询设备属性）。
type BridgeRestore struct {
	Serial      string `json:"serial"`
	ListenHost  string `json:"listen_host"`
	BackendName string `json:"backend_name"` // "adb" | "hdc"
	ADBServer   string `json:"adb_server,omitempty"`
	HDCServer   string `json:"hdc_server,omitempty"`
	AuthMode    string `json:"auth_mode"`
	DeviceID    string `json:"device_id"`
}

// encodeRestoreState 把恢复配置序列化为环境变量可承载的 base64 JSON。
// 环境变量是 exec 传递的载体：ps 可见性不敏感（只有设备与本地后端地址）。
func encodeRestoreState(bridges []BridgeRestore) (string, error) {
	data, err := json.Marshal(bridges)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// DecodeRestoreState 解析 encodeRestoreState 的输出，非法输入返回错误。
func DecodeRestoreState(encoded string) ([]BridgeRestore, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode restore state: %w", err)
	}
	var bridges []BridgeRestore
	if err := json.Unmarshal(data, &bridges); err != nil {
		return nil, fmt.Errorf("parse restore state: %w", err)
	}
	return bridges, nil
}
