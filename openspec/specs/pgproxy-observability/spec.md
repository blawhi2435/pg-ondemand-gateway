# pgproxy-observability Specification

## Purpose
TBD - created by archiving change pg-proxy-phase1. Update Purpose after archive.
## Requirements
### Requirement: 連線稽核事件

pg-proxy SHALL 為每一條連線輸出 `connect` 與 `disconnect` 兩筆 JSON 事件到 stdout，
每筆一行，並以同一個 `conn_id` 串起整條連線。

`connect` MUST 包含：`ts`、`event`、`conn_id`、`cluster`、`sni`、`client_ip`、
`user`、`database`、`application_name`、`backend`、`tls`、`handshake_ms`。

`disconnect` MUST 包含：`ts`、`event`、`conn_id`、`reason`、`bytes_in`、`bytes_out`、`duration_s`。

#### Scenario: user 與 database 永不為空

- **WHEN** 任何一筆 `connect` 事件被輸出
- **THEN** `user` 與 `database` 皆為非空字串

#### Scenario: reason 值域

- **WHEN** 輸出 `disconnect` 事件
- **THEN** `reason` 為 `client_close`、`backend_close`、`backend_error`、
  `idle_timeout`、`shutdown`、`limit_exceeded` 其中之一

#### Scenario: CancelRequest 也有紀錄

- **WHEN** 處理一個 CancelRequest
- **THEN** 輸出一筆 `cancel` 事件（含 `cluster`、`sni`、`client_ip`），
  但不輸出 `connect` / `disconnect`

### Requirement: Prometheus metrics 端點

pg-proxy SHALL 在**獨立於 :5432 的另一個 port** 上提供 `/metrics`，
SHALL NOT 將其掛在資料庫流量的 port 上（該 port 執行的是 PostgreSQL 協定，不是 HTTP）。

MUST 曝出下列指標：

```
pgproxy_connections_active{cluster}              Gauge
pgproxy_connections_total{cluster,result}        Counter
pgproxy_handshake_errors_total{reason}           Counter
pgproxy_backend_dial_seconds{cluster}            Histogram
pgproxy_bytes_total{cluster,direction}           Counter
pgproxy_cert_expiry_seconds                      Gauge
pgproxy_draining                                 Gauge
pgproxy_nofile_limit                             Gauge
pgproxy_route_table_entries                      Gauge
pgproxy_route_table_last_sync_seconds            Gauge
```

#### Scenario: active 反映實際連線數

- **WHEN** 對 tenant1 建立 3 條連線
- **THEN** `pgproxy_connections_active{cluster="tenant1"}` 為 3

#### Scenario: 連線全部結束後歸零

- **WHEN** 所有連線都關閉
- **THEN** `pgproxy_connections_active` 回到 0
  （此項用於偵測連線登記表洩漏）

#### Scenario: 未知 SNI 被計數

- **WHEN** 以未登記的 SNI 連線
- **THEN** `pgproxy_handshake_errors_total{reason="unknown_sni"}` 加 1

#### Scenario: 憑證到期時間可觀測

- **WHEN** 讀取 `pgproxy_cert_expiry_seconds`
- **THEN** 其值對應當前生效憑證的 NotAfter

### Requirement: metrics 標籤基數紀律

metrics 的標籤 SHALL 只使用 `cluster`（以及固定值域的 `result` / `reason` / `direction`）。
SHALL NOT 使用 hostname、user、database、client_ip 或任何其他無界值域作為標籤。

#### Scenario: 不以帳號作為標籤

- **WHEN** 檢視任一指標的標籤集合
- **THEN** 不存在 `user`、`database`、`client_ip` 或 `sni` 標籤

### Requirement: health 與 readiness 端點

pg-proxy SHALL 在 metrics port 上提供 `/healthz` 與 `/readyz`。

`/healthz` SHALL 只反映行程是否存活，SHALL NOT 檢查 informer 狀態——
informer 卡住時重啟 pod 無助於恢復（重啟需要 API server，而它正是可能故障的一方）。

#### Scenario: informer 異常時 healthz 仍為 200

- **WHEN** API server 不可達但 pg-proxy 仍以本地 cache 服務中
- **THEN** `/healthz` 回 200，pod 不被重啟

#### Scenario: cache 未同步時 readyz 為 503

- **WHEN** pg-proxy 啟動但 informer 初始同步尚未完成
- **THEN** `/readyz` 回 503

#### Scenario: drain 期間 readyz 為 503

- **WHEN** pg-proxy 收到 SIGTERM 進入 drain
- **THEN** `/readyz` 回 503，使 Service 盡快將該 pod 移出 endpoints，
  既有連線繼續 drain 至結束

#### Scenario: 不對資料庫 port 做 TCP probe

- **WHEN** 檢查 Deployment 的 probe 設定
- **THEN** liveness 與 readiness 皆指向 metrics port 的 HTTP 端點，
  未對 :5432 設置 TCP probe（那會在 log 產生大量無 SSLRequest 的連線雜訊）

