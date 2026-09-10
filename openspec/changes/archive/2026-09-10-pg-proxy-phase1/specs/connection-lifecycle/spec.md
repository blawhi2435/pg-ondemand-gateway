## ADDED Requirements

### Requirement: 連線處理順序

pg-proxy SHALL 以固定順序處理每一條連線：判斷首包 → TLS 握手 → 查路由表 →
解析 StartupMessage → **呼叫 Authorizer** → 撥後端 → 轉送認證訊息至 AuthenticationOk →
登記連線並輸出 connect 事件 → 雙向轉發 → 註銷並輸出 disconnect 事件。

#### Scenario: Authorizer 在撥後端之前被呼叫

- **WHEN** 一條連線完成 StartupMessage 解析
- **THEN** 在建立任何後端連線**之前**呼叫 `Authorizer.Check(cluster, user, database, clientIP)`

#### Scenario: 第一階段永遠放行

- **WHEN** 第一階段的 Authorizer 被呼叫
- **THEN** 一律回傳 allow，且不查詢任何外部系統

#### Scenario: 認證完成後才登記

- **WHEN** 後端尚未回覆 `AuthenticationOk`
- **THEN** 該連線尚未進入登記表，也尚未輸出 connect 事件

### Requirement: 對後端使用標準 TLS 交涉

pg-proxy SHALL 以標準 PostgreSQL TLS 交涉連接後端（送 `SSLRequest`、等 `'S'`、再握手），
SHALL NOT 使用 direct TLS（那會額外要求 PgBouncer ≥ 1.25）。
後端憑證 SHALL 以 CNPG 的內部 CA 驗證。

#### Scenario: 後端連不上

- **WHEN** 撥後端逾時或被拒絕
- **THEN** 對 client 回覆 SQLSTATE `08006` 的 ErrorResponse，
  並將 `pgproxy_connections_total{result="backend_error"}` 加 1

### Requirement: 連線登記表

pg-proxy SHALL 維護一份可列舉的連線登記表，每一筆 SHALL 包含
`conn_id`、`cluster`、`sni`、`client_ip`、`user`、`database`、`application_name`、
`backend`、`started_at`、`revoked`，以及 client 與 backend 兩側的 socket。
SHALL NOT 只保存計數。

#### Scenario: 可列舉

- **WHEN** 呼叫 Snapshot
- **THEN** 回傳所有進行中連線的完整中繼資料

#### Scenario: 註銷後計數歸零

- **WHEN** 所有連線都結束
- **THEN** 總數與每個 cluster 的計數皆為 0

### Requirement: 連線上限

pg-proxy SHALL 施加全域與 per-cluster 兩層連線上限，兩者皆為 **per-pod**。
超限時 SHALL 回覆 SQLSTATE `53300` 的 ErrorResponse。

#### Scenario: 超過 per-cluster 上限

- **WHEN** 某 cluster 的進行中連線數已達 `limits.maxConnsPerCluster`，又有新連線進入
- **THEN** 回覆 `53300`，該連線不進入登記表，
  `pgproxy_connections_total{result="limit_exceeded"}` 加 1

#### Scenario: 超過全域上限

- **WHEN** 總連線數已達 `limits.maxConns`
- **THEN** 不論屬於哪個 cluster 皆回覆 `53300`

### Requirement: 啟動時提高 nofile 上限

pg-proxy SHALL 在啟動時將 `RLIMIT_NOFILE` 的 soft limit 提高到 hard limit，
並 SHALL 將結果寫入啟動 log 且曝為 metric。
理由是 k8s 的 pod spec 沒有設定 nofile 的欄位，實際值由容器 runtime 預設決定且各叢集不同。

#### Scenario: 啟動時自行提高

- **WHEN** pg-proxy 啟動且 soft limit 低於 hard limit
- **THEN** soft limit 被設為 hard limit，`pgproxy_nofile_limit` 反映最終值，
  並在啟動 log 記錄提高前後的數值

### Requirement: Graceful shutdown

收到 `SIGTERM` 後，pg-proxy SHALL 立即停止接受新連線、
讓既有連線自然結束，並在 `shutdown.drainTimeout` 到期後才強制關閉剩餘連線。

#### Scenario: 停收新連線但既有連線繼續

- **WHEN** 收到 SIGTERM
- **THEN** listener 關閉、`pgproxy_draining` 設為 1、readiness 端點回 503，
  既有連線繼續正常轉發

#### Scenario: 全部結束後即退出

- **WHEN** drain 期間所有連線都自然結束
- **THEN** 行程立即退出，不等到 `drainTimeout` 屆滿

#### Scenario: 逾時後強制關閉

- **WHEN** `drainTimeout` 屆滿仍有連線存在
- **THEN** 關閉剩餘連線，輸出 `reason: shutdown` 的 disconnect 事件，然後退出

#### Scenario: 滾動更新不中斷既有連線

- **WHEN** 對 pg-proxy Deployment 執行滾動更新，且期間存在長連線
- **THEN** 既有連線不被中斷（前提是 `terminationGracePeriodSeconds > drainTimeout`）

### Requirement: idle timeout

pg-proxy SHALL 在雙向皆無流量超過 `timeouts.idle` 時關閉該連線，
並以 `reason: idle_timeout` 輸出 disconnect 事件。

#### Scenario: 閒置逾時關閉

- **WHEN** 一條連線在 `timeouts.idle` 期間兩個方向都沒有位元組流動
- **THEN** 兩側 socket 皆被關閉，並輸出 `reason: idle_timeout`

### Requirement: 轉發實作可替換

雙向轉發 SHALL 收斂在一個介面之後，使其能在不更動連線管理、逾時與關閉順序的前提下，
被替換為未來的訊息解析實作。

#### Scenario: 任一端關閉即結束

- **WHEN** client 或 backend 任一端關閉連線
- **THEN** 另一端亦被關閉，並回報傳輸位元組數與結束原因
