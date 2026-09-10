# pgproxy-deployment Specification

## Purpose
TBD - created by archiving change pg-proxy-phase1. Update Purpose after archive.
## Requirements
### Requirement: 測試環境與既有工作負載隔離

所有本次交付的 k8s 物件 SHALL 建立在專屬的 namespace 內，
SHALL NOT 修改或刪除既有的 `cnpg-system`、`training` 或任何其他既有 namespace 的物件。

#### Scenario: 只動自己的 namespace

- **WHEN** 執行 setup 或 teardown 腳本
- **THEN** 只有專屬 namespace 與 APISIX 的 helm release 受影響

### Requirement: APISIX 為純 L4 轉發

APISIX SHALL 以 stream route 對 pg-proxy 做純 TCP 轉發，
SHALL NOT 在 stream route 上設定 `sni`，SHALL NOT 終止 TLS。

理由：PostgreSQL 的第一個封包是 `SSLRequest` 而非 TLS ClientHello，APISIX 看不到 SNI；
若設定 `sni`，APISIX 會嘗試終止 TLS，使整條 TLS 鏈路失效。

#### Scenario: 單一 stream route

- **WHEN** 檢視 APISIX 的 stream route 設定
- **THEN** 只有一條 route，upstream 為 pg-proxy Service:5432，且未設定 `sni`

#### Scenario: TLS 由 pg-proxy 終止

- **WHEN** client 以 `verify-full` 成功連線
- **THEN** 其驗證通過的憑證是 pg-proxy 持有的 wildcard 憑證
  （該憑證不存在於 APISIX，因此連線成功即證明 TLS 在 pg-proxy 終止）

### Requirement: 高可用部署

pg-proxy SHALL 以 Deployment 部署，replica 數 SHALL 為 3，
並 SHALL 配置 `PodDisruptionBudget(minAvailable: 2)`。
Service SHALL 為一般 ClusterIP，SHALL NOT 使用 session affinity。

`terminationGracePeriodSeconds` MUST 大於 `shutdown.drainTimeout`。

#### Scenario: PDB 不阻塞滾動更新

- **WHEN** 以 3 replica 與 `minAvailable: 2` 執行滾動更新
- **THEN** 更新可正常完成，不發生因 PDB 而卡死的情況

#### Scenario: 無狀態、可跨 replica

- **WHEN** 一個 CancelRequest 落到與原查詢不同的 replica
- **THEN** 依 SNI 轉發後查詢仍被成功取消

### Requirement: TLS 憑證以 k8s Secret 提供

pg-proxy 的對外憑證 SHALL 以 `kubernetes.io/tls` 型別的 Secret 掛載提供。
憑證的簽發來源 SHALL NOT 由 pg-proxy 感知或依賴——
無論來自公司既有 CA、手動建立或任何自動化工具，介面都是同一個 Secret。

憑證 MUST 包含 `subjectAltName = DNS:*.<domain>`；僅設定 CN 不足以通過 `verify-full`。

#### Scenario: 以 Secret 更換憑證

- **WHEN** 以新的憑證內容更新該 Secret
- **THEN** 不需重建 Deployment，新連線即用到新憑證

### Requirement: 對後端使用 CNPG 內部 CA

pg-proxy 驗證後端 Pooler 時 SHALL 使用 CNPG 自動產生的內部 CA Secret，
與對外的 wildcard 憑證完全分離。CNPG Cluster SHALL NOT 需要設定 `serverTLSSecret`。

#### Scenario: 兩段憑證互相獨立

- **WHEN** 更換對外的 wildcard 憑證
- **THEN** 對後端的連線不受影響

### Requirement: 端到端驗收

交付 MUST 包含可重複執行的 e2e 驗收腳本，涵蓋下列情境並全數通過。

#### Scenario: 正向連線與分流

- **WHEN** 以 `verify-full` 分別連線 tenant1 與 tenant2 的 hostname
- **THEN** 兩者皆成功，且分別到達不同的後端

#### Scenario: 錯誤的 CA 必須失敗

- **WHEN** client 以一個不相干的 CA 作為 root 憑證進行 `verify-full`
- **THEN** 連線失敗
  （此項證明測試確實在驗證憑證，而非等同於 `sslmode=require`）

#### Scenario: 未知 SNI 被明確拒絕

- **WHEN** 連線一個不在路由表中的 hostname
- **THEN** 連線被明確拒絕而非逾時掛起

#### Scenario: 超過上限回 53300

- **WHEN** 對單一 cluster 建立超過 `maxConnsPerCluster` 的連線
- **THEN** 超出的連線收到 SQLSTATE `53300`

#### Scenario: 憑證熱重載

- **WHEN** 將 Secret 更新為第二張憑證
- **THEN** 新連線使用新憑證，且更新前建立的連線不中斷

#### Scenario: 動態新增與移除租戶

- **WHEN** 新增一個帶 annotation 的 Pooler，之後再刪除它
- **THEN** 新增後不需重啟即可連線；刪除後新連線被拒而既有連線存活

#### Scenario: 滾動更新不中斷

- **WHEN** 在存在長連線的情況下執行 `kubectl rollout restart`
- **THEN** 既有連線不中斷

#### Scenario: 查詢取消

- **WHEN** 對一個進行中的長查詢送出取消
- **THEN** 查詢確實被取消

#### Scenario: metrics 歸零

- **WHEN** 建立數條連線後全部關閉
- **THEN** `pgproxy_connections_active` 回到 0

### Requirement: 測試環境上限需可被觸及

測試環境的連線上限設定 SHALL 小到能在驗收中實際觸發，
SHALL NOT 沿用正式環境的數值（無法觸發的上限等同於未經測試）。

#### Scenario: 上限可被測到

- **WHEN** 執行超過上限的驗收情境
- **THEN** 能在測試環境資源限制內實際收到 `53300`

### Requirement: 完整拆除

交付 MUST 包含拆除腳本，執行後 SHALL 移除本次建立的所有資源，
並使叢集回到執行 setup 之前的狀態。

#### Scenario: 拆除後無殘留

- **WHEN** 執行 teardown 腳本
- **THEN** 專屬 namespace 與 APISIX release 均已移除，
  既有的 `cnpg-system` 與 `training` 完好無損

#### Scenario: 可重複安裝

- **WHEN** 在 teardown 之後再次執行 setup
- **THEN** 環境可完整重建，驗收可再次通過

### Requirement: PROXY protocol 的實測與誠實記錄

pg-proxy SHALL 實作 PROXY protocol v1/v2 的解析，並以設定項控制，預設關閉。
APISIX 對 upstream 是否能傳送 PROXY protocol SHALL 以實測確認，
SHALL NOT 假設其可用。

#### Scenario: 實測結果被記錄

- **WHEN** 完成 APISIX 的 PROXY protocol 實測
- **THEN** 結果（含 APISIX 版本與設定方式）寫入交付文件；
  若不支援，則 `client_ip` 記錄的是 APISIX pod IP 一事被列為已知限制

