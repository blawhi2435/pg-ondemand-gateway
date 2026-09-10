## ADDED Requirements

### Requirement: 憑證熱重載

pg-proxy SHALL 在不重啟的前提下換用新的 TLS 憑證。
憑證 SHALL 透過 `tls.Config.GetCertificate` 於**每次握手時**取得當前值。

#### Scenario: 新連線用到新憑證

- **WHEN** 掛載的 TLS Secret 內容被更新，且已過一個重載間隔
- **THEN** 之後建立的連線收到新憑證

#### Scenario: 既有連線不受影響

- **WHEN** 憑證在某條連線建立之後被更換
- **THEN** 該連線繼續正常運作、不被中斷、不重新握手

#### Scenario: 壞掉的憑證不覆蓋既有值

- **WHEN** 重讀時檔案內容無法解析為合法的憑證/私鑰對
- **THEN** 保留原先的憑證繼續服務，記錄錯誤並增加錯誤計數，
  SHALL NOT 換上不可用的憑證

### Requirement: 設定熱重載

以下設定項 SHALL 可在不重啟的前提下生效：
`limits.maxConns`、`limits.maxConnsPerCluster`、
`timeouts.handshake`、`timeouts.backendDial`、`timeouts.idle`、
`shutdown.drainTimeout`。

`listen`、`metricsListen` 與 TLS 檔案路徑 SHALL 需要重啟才生效。

#### Scenario: 上限調整即時生效

- **WHEN** ConfigMap 中的 `limits.maxConnsPerCluster` 被調高，且已過一個重載間隔
- **THEN** 後續新連線依新的上限判斷，不需重啟

#### Scenario: 既有連線不受設定變更影響

- **WHEN** 設定在連線建立之後變更
- **THEN** 該連線繼續使用建立當下的值，不被中斷

#### Scenario: 無效設定不覆蓋既有值

- **WHEN** 重讀到的設定無法解析，或含有不合法的值（例如負數逾時）
- **THEN** 保留原先設定繼續服務並記錄錯誤

### Requirement: 以定期重讀偵測變更

pg-proxy SHALL 以「定期重讀檔案 + 內容雜湊比對 + 原子替換」偵測並套用變更，
SHALL NOT 依賴檔案系統事件通知（fsnotify）。

理由：k8s 更新掛載的 Secret/ConfigMap 是建立新的 `..data` 目錄再原子替換 symlink，
對單一檔案掛 watch 收不到寫入事件，錯誤實作會靜默失效；
而 kubelet 的同步本身即有約 60 秒延遲，事件通知省下的延遲沒有實益。

#### Scenario: 內容未變不進行替換

- **WHEN** 重讀時檔案內容的雜湊與前次相同
- **THEN** 不執行替換，不產生任何 log

#### Scenario: 內容改變才替換

- **WHEN** 雜湊與前次不同
- **THEN** 原子替換當前值，並記錄一筆變更事件

### Requirement: 掛載方式限制

TLS Secret 與設定 ConfigMap SHALL 以目錄形式掛載，SHALL NOT 使用 `subPath`。
使用 `subPath` 掛載的檔案在 mount 之後永不更新，會使熱重載靜默失效。
此限制 MUST 於 manifest 中以註解標示。

#### Scenario: manifest 不使用 subPath

- **WHEN** 檢查 pg-proxy Deployment 的 volumeMounts
- **THEN** TLS 與設定的掛載皆未使用 `subPath`，且帶有說明此限制的註解
