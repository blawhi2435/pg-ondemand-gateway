## Why

Cluster A 的應用要連到 Cluster B 各租戶的 CloudNativePG Pooler。PostgreSQL 的第一個封包是
`SSLRequest` 而不是 TLS ClientHello，所以 nginx / APISIX 這類 L4 代理看不到 SNI、無法分流——
現成元件解不了這個問題（推導見 `cross-cluster-pooler.html`、`edge-option-decision.html`）。

自建一個懂 PostgreSQL 協定的 proxy：代答 SSLRequest、終止 TLS 後取得 SNI、依租戶分流到對應 Pooler，
client 端零改動。

本次只做第一階段：**終止 TLS、分流、記錄、轉發，不做任何判斷**。權限控制（讀 Redis、連線層撤銷）
是第二階段，但第一階段必須把四個接縫留好，否則第二階段是重寫而不是加實作。

第一階段還有一個獨立於程式的產物：`connect` 事件會產生「實際上有哪些帳號在連哪個資料庫」的真實清單。
第二階段開強制之前要拿它跟申請紀錄對帳，沒有這一步，開強制那一刻會有一批人一起斷線。

## What Changes

- 新增 Go 服務 `pg-proxy`：代答 SSLRequest、以 wildcard 憑證終止 TLS、依 SNI 查路由表、
  解析 StartupMessage（**解析但不判斷**）、對後端做標準 PostgreSQL TLS 交涉、雙向轉發。
- 路由表來源為 **CNPG 既有的 Pooler CR**（加兩個 annotation），用 dynamic informer 監聽並整份原子換掉。
  **不新增任何 CRD**。
- 四個接縫以介面定義並就定位：Router / Authorizer / Registry / Relay。
  Authorizer 第一階段是永遠回 allow 的空實作，但呼叫點必須在「解析完 StartupMessage、撥後端之前」。
- 五項上線周邊：graceful shutdown、憑證與設定熱重載、連線上限（含啟動時自拉 nofile）、
  後端故障與 idle timeout、Prometheus metrics。
- k8s 交付物與端到端驗證環境：APISIX（純 L4 stream route）、兩組 CNPG Cluster + Pooler、
  pg-proxy Deployment(3) + PDB + RBAC + PodMonitor、測試 client、憑證產生與拆除腳本。

**非破壞性**：這是全新服務，repo 目前只有設計文件，沒有既有程式碼會被改動。

## Capabilities

### New Capabilities

- `pgwire-protocol`: PostgreSQL wire protocol 3.0 的位元組層處理——首包四種型別的判斷
  （SSLRequest / GSSENCRequest / CancelRequest / 明文 StartupMessage）、StartupMessage 的
  鍵值對解析、ErrorResponse 的產生與 SQLSTATE 對應。
- `sni-routing`: 從 Pooler CR annotation 建立並維護 SNI → (cluster, 後端位址) 的路由表。
  涵蓋 informer 的 List-then-Watch、整份原子替換、hostname 撞名與多層 label 的拒絕、
  以及啟動/API server 故障/Pooler 刪除各自的行為契約。
- `connection-lifecycle`: 一條連線從 accept 到關閉的完整流程（11 步）、四個接縫的介面、
  連線登記表、全域與 per-cluster 上限、graceful drain。
- `config-hot-reload`: TLS 憑證與執行期設定的熱重載——定期重讀 + 雜湊比對 + 原子替換，
  以及「既有連線不受影響、只有新連線用新值」的契約。
- `pgproxy-observability`: 每條連線的 connect / disconnect JSON 稽核事件、
  Prometheus metrics（含 cardinality 紀律）、health 與 readiness 端點的語意。
- `pgproxy-deployment`: k8s 部署與端到端驗證環境——manifests、RBAC、憑證產生、
  APISIX L4 設定、e2e 驗收腳本、完整拆除。

### Modified Capabilities

（無。`openspec/specs/` 目前為空，這是本專案第一個 change。）

## Impact

**新增程式碼**（`github.com/blawhi2435/pg-ondemand-gateway`）

```
cmd/pg-proxy/          internal/pgwire/   internal/route/    internal/authz/
internal/registry/     internal/relay/    internal/config/   internal/tlsutil/
internal/audit/        internal/metrics/
```

**新增相依**：`k8s.io/client-go`（dynamic informer）、`k8s.io/apimachinery`、
`github.com/prometheus/client_golang`。刻意**不**引入 CNPG 的 Go module——
只需要 metadata 的兩個 annotation，用 typed client 會帶進整包版本相依。

**外部系統**

| 系統 | 影響 |
|---|---|
| CNPG Pooler CR | 讀取兩個 annotation。**唯讀**，pg-proxy 永不寫入 |
| k8s API server | 每個 pod 一條 watch 長連線（idle 時零流量） |
| APISIX | 新增一條 L4 stream route。**不可設 `sni`**，設了會嘗試終止 TLS |
| Prometheus | 新增一個 scrape target（PodMonitor） |

**已知限制**（照實記錄，不假裝解決）

1. 來源 IP 可能被改寫——APISIX 對 upstream 能否帶 PROXY protocol 是本次的實測項目；
   PgBouncer 本身不支援，所以 PostgreSQL 端看到的一定是 pooler pod IP。
2. idle timeout 需與雲端 LB 對齊，本機環境只能驗機制、無法驗數字。
3. SCRAM channel binding 不可用——TLS 兩段重建，`channel_binding=require` 會失敗。

**不在本次範圍**：Redis 與權限判斷、SQL 語句層稽核、中央服務那一側、正式環境部署。
