# sni-routing Specification

## Purpose
TBD - created by archiving change pg-proxy-phase1. Update Purpose after archive.
## Requirements
### Requirement: 路由表來源為 Pooler CR annotation

pg-proxy SHALL 從 CloudNativePG 既有的 Pooler CR 建立路由表，
讀取 `pg-proxy.internal/hostname` 與 `pg-proxy.internal/cluster` 兩個 annotation。
pg-proxy SHALL NOT 定義任何新的 CRD，且 SHALL NOT 對 Pooler 做任何寫入。

#### Scenario: 帶 annotation 的 Pooler 進入路由表

- **WHEN** 存在一個 Pooler 帶有 `pg-proxy.internal/hostname: tenant1.db.test`
  與 `pg-proxy.internal/cluster: tenant1`
- **THEN** 路由表含有 `tenant1.db.test → (cluster=tenant1, addr=<name>.<namespace>.svc:5432)`

#### Scenario: 未帶 annotation 的 Pooler 被略過

- **WHEN** 一個 Pooler 沒有 `pg-proxy.internal/hostname` annotation
- **THEN** 該 Pooler 不進入路由表，且不產生錯誤

### Requirement: 路由查詢回傳 cluster 識別字

Router 的查詢介面 SHALL 同時回傳 cluster 識別字與後端位址，
SHALL NOT 只回傳位址。cluster 識別字用於稽核 log，並且是第二階段組出
`pgproxy:policy:{cluster}` 這個 Redis key 的唯一來源。

#### Scenario: 查得到

- **WHEN** 以 SNI `tenant1.db.test` 查詢
- **THEN** 回傳 `(cluster="tenant1", addr="tenant1-pooler.pgproxy-e2e.svc:5432", found=true)`

#### Scenario: 查不到

- **WHEN** 以未登記的 SNI 查詢
- **THEN** 回傳 `found=false`，呼叫端關閉連線並將
  `pgproxy_handshake_errors_total{reason="unknown_sni"}` 加 1

### Requirement: hostname 必須是單層 label

pg-proxy SHALL 拒絕多層的 hostname。wildcard 憑證 `*.db.test` 只涵蓋一層，
`a.b.db.test` 的 TLS 驗證必然失敗，因此這種設定必須在載入時就被擋下並可見。

#### Scenario: 多層 hostname 被拒絕

- **WHEN** 某 Pooler 宣告 `pg-proxy.internal/hostname: a.b.db.test`
- **THEN** 該筆不進入路由表，並對該 Pooler 發出 `InvalidHostname` 事件

### Requirement: hostname 撞名時拒絕後者

當兩個 Pooler 宣告同一個 hostname 時，pg-proxy SHALL 保留先處理到的那一筆、
拒絕後者，並發出事件。SHALL NOT 靜默覆蓋。

#### Scenario: 撞名被拒絕並發事件

- **WHEN** 兩個 Pooler 都宣告 `tenant1.db.test`
- **THEN** 路由表只保留一筆，另一個 Pooler 收到 `DuplicateHostname` 事件

#### Scenario: 前者刪除後由後者接手

- **WHEN** 撞名的兩個 Pooler 中，先前被保留的那一個被刪除
- **THEN** 下一次重建後，原本被拒絕的那一個進入路由表

### Requirement: 路由表更新必須整份原子替換

路由表 SHALL 以「重建整份 map 後原子替換指標」的方式更新，
SHALL NOT 就地修改。讀取路徑（每條新連線）SHALL NOT 需要取得任何鎖。

#### Scenario: 併發讀寫不觸發競態

- **WHEN** 在 `-race` 下同時進行大量路由查詢與多次路由表重建
- **THEN** 不出現 data race 報告，也不出現 `concurrent map read and map write` panic

### Requirement: informer 生命週期契約

pg-proxy SHALL 以 List-then-Watch 的方式維護路由表，並遵守以下行為契約。

#### Scenario: 啟動時先同步再開 listener

- **WHEN** pg-proxy 啟動
- **THEN** 先完成初始 List 與 cache 同步，**之後**才開啟 :5432 的 listener；
  在同步完成前，readiness 端點回 503

#### Scenario: 啟動時 API server 連不上

- **WHEN** 初始 List 失敗
- **THEN** 行程以非零狀態結束（交由 k8s 重試），
  SHALL NOT 以空路由表繼續執行並顯示為健康

#### Scenario: 執行中 API server 中斷

- **WHEN** pg-proxy 已在服務中，API server 變為不可達
- **THEN** 繼續以本地 cache 服務既有與新連線，
  且 `pgproxy_route_table_last_sync_seconds` 持續增長以供告警

#### Scenario: 新增 Pooler 即時生效

- **WHEN** 套用一個帶 annotation 的新 Pooler
- **THEN** 不需重啟 pg-proxy，該 hostname 隨即可連線，
  `pgproxy_route_table_entries` 加 1

### Requirement: 路由變動不得主動中斷既有連線

當 Pooler 被刪除或 annotation 被移除時，pg-proxy SHALL 僅將該筆自路由表移除，
SHALL NOT 主動關閉任何已建立的連線。

#### Scenario: 刪除 Pooler 後既有連線存活

- **WHEN** 某 Pooler 被刪除，而該租戶尚有進行中的連線
- **THEN** 新連線被拒（unknown SNI），既有連線繼續運作，
  直到後端消失後自然收到 EOF 並以 `reason: backend_close` 結束

### Requirement: 唯讀的最小權限

pg-proxy 使用的 ServiceAccount SHALL 只被授予 Pooler 的 `get`/`list`/`watch`
與 Event 的 `create`，SHALL NOT 具備對 Pooler 的任何寫入權限。

#### Scenario: 權限足以 watch

- **WHEN** 執行 `kubectl auth can-i watch poolers.postgresql.cnpg.io --as=<sa>`
- **THEN** 結果為 `yes`

#### Scenario: 無寫入權限

- **WHEN** 執行 `kubectl auth can-i update poolers.postgresql.cnpg.io --as=<sa>`
- **THEN** 結果為 `no`

