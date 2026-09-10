# pg-proxy 第一階段 — 設計規格

- 日期：2026-09-09
- 模組路徑：`github.com/blawhi2435/pg-ondemand-gateway`
- 需求來源：`pg-proxy-phases.html` §0–§6、`pg-proxy-architecture.html` §4–§6

## 1. 目標

實作 pg-proxy 的第一階段，並在 OrbStack 的 k8s 上完成端到端驗證：

```
psql --sslmode=verify-full  →  APISIX (L4)  →  pg-proxy  →  CNPG Pooler  →  PostgreSQL
```

pg-proxy 在這條路徑上做四件事：終止 TLS、依 SNI 分流、解析 StartupMessage（不做判斷）、雙向轉發。
外加五項決定「能不能上線」的周邊：graceful shutdown、憑證/設定熱重載、連線上限、後端故障處理、metrics。

完成的定義：`§10 驗收矩陣` 全部通過，且測試環境可用腳本完整拆除。

## 2. 非目標

- **第二階段的權限控制**。不連 Redis、不做任何拒絕判斷。接縫 B 是永遠回 allow 的空實作，但呼叫點必須就位。
- **SQL 語句層的稽核或攔截**。轉發之後不解析訊息內容。
- **中央服務那一側**。誰把資料寫進 Redis、資料新鮮度，都不在範圍內。
- **正式環境部署**。本規格只交付到「在 OrbStack 上跑通並驗收」。

## 3. 架構

```
psql (verify-full, PGSSLROOTCERT=testca.crt)
  │  TLS：測試 CA 簽發的 *.db.test
  ▼
APISIX  ── 純 L4 stream route，單一條，不終止 TLS
  ▼
pg-proxy  Deployment × 3 + PDB(minAvailable: 2)
  │  ① 代答 SSLRequest → 用 wildcard 憑證終止 TLS
  │  ② SNI → 查路由表 → (cluster, 後端位址)
  │  ③ 解析 StartupMessage → user / database / application_name（不判斷）
  │  ④ 標準 PostgreSQL TLS 交涉 → 雙向轉發
  │  TLS：CNPG 內部 CA
  ▼
tenant1-pooler / tenant2-pooler  (PgBouncer)
  ▼
CNPG Cluster tenant1-db / tenant2-db
```

**APISIX 必須是純 L4。** PostgreSQL 的第一個封包是 `SSLRequest` 而不是 TLS ClientHello，
所以 APISIX 看不到 SNI、做不了分流。stream route 只有一條，upstream 就是 pg-proxy Service。
**stream route 上不可設 `sni`**——設了 APISIX 會嘗試終止 TLS，整條鏈就錯了。

**兩段 TLS 使用完全不同的 CA：**

| 段 | 憑證 | 誰管 |
|---|---|---|
| client → pg-proxy | `*.db.test` wildcard，測試 CA 簽 | 我們（正式環境 = 公司發的憑證） |
| pg-proxy → Pooler | CNPG 自簽的內部 CA | CNPG operator 自動產生 |

wildcard 憑證只存在於 pg-proxy，Cluster 完全不需要設 `serverTLSSecret`。

## 4. 程式碼佈局

package 邊界就是接縫的邊界。

```
cmd/pg-proxy/main.go       組裝、設定載入、生命週期
internal/pgwire/           首包判斷、StartupMessage、ErrorResponse
internal/route/            接縫 A：Router（Pooler CR informer）
internal/authz/            接縫 B：Authorizer（allowAll）
internal/registry/         接縫 C：連線登記表 + 上限
internal/relay/            接縫 D：Relay（io.Copy）
internal/config/           ConfigMap 熱重載
internal/tlsutil/          憑證熱重載
internal/audit/            connect / disconnect JSON 事件
internal/metrics/          Prometheus registry 與 /metrics
```

## 5. 四個接縫

### A · Router

```go
type Route struct {
    Cluster string   // 二階段拿它組 Redis key，一階段寫進 log
    Addr    string   // host:port
}

type Router interface {
    Lookup(sni string) (Route, bool)
}
```

**回傳值必須包含 cluster 識別字**，不能只回位址。第二階段用它組 `pgproxy:policy:{cluster}`。

實作：CNPG Pooler CR 的 dynamic informer（見 §7）。

### B · Authorizer

```go
type Decision struct {
    Allow    bool
    SQLState string   // 拒絕時填
    Reason   string
}

type Authorizer interface {
    Check(cluster, user, database, clientIP string) Decision
}
```

第一階段實作是 `allowAll{}`，永遠回 `Decision{Allow: true}`。

**呼叫點必須在解析完 StartupMessage 之後、撥後端之前**（流程第 6 步）。
放在撥完後端才檢查的話，第二階段每次拒絕都會浪費一次後端連線，錯誤處理也會變複雜。

### C · Registry

```go
type Conn struct {
    ConnID          string
    Cluster         string
    SNI             string
    ClientIP        string
    User            string
    Database        string
    ApplicationName string
    Backend         string
    StartedAt       time.Time
    Revoked         bool          // 第一階段不使用，第二階段撤銷時標記
    client, backend net.Conn      // 第二階段撤銷要關兩側
}

type Registry interface {
    Add(*Conn) error              // 超過上限回 ErrTooManyConns
    Remove(connID string)
    Snapshot() []*Conn
    Count() (total int, perCluster map[string]int)
}
```

**不能只存計數。** 上限、graceful drain、以及第二階段的撤銷掃描都靠它列舉。
`Revoked` 欄位第一階段用不到，但現在補是零成本，之後補是每個地方都要改。

### D · Relay

```go
type Relay interface {
    Run(ctx context.Context, client, backend net.Conn) (bytesIn, bytesOut int64, reason string)
}
```

第一階段是雙向 `io.Copy`，用 `sync.Pool` 管 buffer。
連線管理、逾時、關閉順序留在外圍，之後換成訊息解析實作時外圍不用動。

## 6. 連線流程

```
1.  accept；設 TCP_NODELAY 與握手 deadline（timeouts.handshake）
2.  讀 8 bytes → 判斷首包型別
3.  SSLRequest → 回 'S' → TLS 握手（GetCertificate 動態取憑證）
4.  Router.Lookup(sni) → Route                              ← 接縫 A
5.  讀 StartupMessage → user / database / application_name
6.  Authorizer.Check(...) → 第一階段永遠 allow              ← 接縫 B
7.  撥後端；標準 SSLRequest 交涉；原樣送出 StartupMessage
8.  雙向轉送認證訊息，等到 AuthenticationOk
9.  Registry.Add + 輸出 connect 事件                        ← 接縫 C
10. Relay.Run                                               ← 接縫 D
11. defer：Registry.Remove + 輸出 disconnect 事件
```

**第 8 步等 `AuthenticationOk` 才登記**：在那之前 client 宣稱的 user 還沒被驗證，
log 的可信度取決於這個順序。

### 6.1 首包的四種可能

前 8 bytes，都是 big-endian int32：

```
length=8   code=80877103  → SSLRequest      回 'S'，然後 TLS 握手
length=8   code=80877104  → GSSENCRequest   回 'N'，client 會退回一般流程
length=16  code=80877102  → CancelRequest   再讀 8 bytes（pid + secret）
length=N   code=196608    → 明文 StartupMessage（協定 3.0）
```

**CancelRequest 必須處理，這是最容易漏的一項。** 它走一條全新的 TCP 連線，
沒有認證、沒有 user 欄位，只帶要取消的後端 pid 與 secret key。
TLS 已經終止，所以 SNI 仍然告訴我們要轉給哪個 Pooler——轉過去讓 PgBouncer 處理即可，
**不要套用後續任何流程**（不登記、不做權限判斷），記一筆稽核事件就好。
多副本沒問題，落在哪個 replica 都能轉。

漏掉的症狀是「查詢按 Ctrl-C 沒有反應」，通常到正式環境才發現。

明文 StartupMessage（沒有先送 SSLRequest）代表 client 用 `sslmode=disable`。
第一階段的處理：拒絕並回 ErrorResponse，理由是 proxy 只接受 TLS。

### 6.2 StartupMessage payload

以 `\0` 結尾的字串成對出現，最後多一個 `\0`：

```
"user\0app_rw\0database\0orders\0application_name\0metabase\0\0"
```

`database` 未提供時預設等於 `user`（PostgreSQL 的慣例）。

### 6.3 ErrorResponse 位元組格式

```
'E'                  1 byte
int32 length         # 含自己，不含 type byte
'S' "FATAL" \0       # severity（會被本地化的舊欄位）
'V' "FATAL" \0       # severity（不本地化，PG 9.6+）
'C' "<SQLSTATE>" \0
'M' "<訊息>" \0
\0                   # 欄位結束
```

寫完就關閉連線。**不要用靜默 RST 拒絕**——client 端會變成難以診斷的逾時。

| SQLSTATE | 用在 |
|---|---|
| `08006` | 後端連不上、後端握手失敗 |
| `53300` | 超過連線上限 |
| `28000` | （第二階段）帳號或 database 沒有授權 |
| `57P01` | （第二階段）撤銷既有連線 |

### 6.4 對後端的連線

送 `SSLRequest`、等 `'S'`、再握手（**標準交涉，不要 direct TLS**——那會多帶一個
PgBouncer ≥ 1.25 的版本要求）。用 CNPG 的內部 CA 驗證後端憑證。

## 7. 路由表（接縫 A 實作）

### 7.1 資料來源

**不建立任何新的 CRD。** 使用 CNPG 既有的 Pooler CR，在上面加兩個 annotation：

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Pooler
metadata:
  name: tenant1-pooler
  namespace: pgproxy-e2e
  annotations:
    pg-proxy.internal/hostname: tenant1.db.test
    pg-proxy.internal/cluster:  tenant1
spec:
  cluster: {name: tenant1-db}
  instances: 2
  type: rw
```

CNPG 會忽略它不認識的 annotation。新增租戶時只要建 Pooler 本身。

**hostname 必須是單層 label**：wildcard 憑證 `*.db.test` 只涵蓋一層，
`a.b.db.test` 驗證會失敗。載入時要驗證並拒絕多層 hostname。

### 7.2 informer

用 `k8s.io/client-go/dynamic` 的 **dynamic informer**，不要 typed client。
我們只需要 metadata 的 name / namespace / annotations，用不到 Pooler 的 spec；
typed client 會把整個 CNPG Go module 拉進來，徒增版本相依。

機制：啟動時 List 建立初始表 → 接上 watch → 有事件就**整份重建再原子換掉**。
watch 是一條由 pg-proxy 主動開啟的長連線（API server 從不主動連 pg-proxy），
idle 時零流量。斷線、`410 Gone` 由 informer 自動處理（退回完整 List）。

### 7.3 併發更新（`pg-proxy-architecture.html` §6-04）

```go
type table map[string]Route          // sni → Route
var current atomic.Pointer[table]

// 更新路徑：informer 事件觸發，整份重建
func rebuild(poolers []*unstructured.Unstructured) {
    t := make(table)
    for _, p := range poolers {
        host := annotation(p, "pg-proxy.internal/hostname")
        if host == "" { continue }
        if err := validateSingleLabel(host); err != nil {
            emitEvent(p, "InvalidHostname", err.Error()); continue
        }
        if _, dup := t[host]; dup {
            emitEvent(p, "DuplicateHostname", host)   // 拒絕，不要靜默覆蓋
            continue
        }
        t[host] = Route{
            Cluster: annotation(p, "pg-proxy.internal/cluster"),
            Addr:    p.GetName() + "." + p.GetNamespace() + ".svc:5432",
        }
    }
    current.Store(&t)
}

// 讀取路徑：每條新連線，無鎖
func (r *informerRouter) Lookup(sni string) (Route, bool) {
    route, ok := (*current.Load())[sni]
    return route, ok
}
```

**必須整份換，不可就地修改。** Go 的 map 不是併發安全的，
informer 一邊寫、連線一邊讀會觸發 `concurrent map read and map write` panic。
整份重建後換指標，讀取路徑完全不用鎖——而讀取是每條新連線都要走的熱路徑。

整份重建的另一個好處：兩個 Pooler 撞 hostname 時我們拒絕後者；等第一個被刪掉，
下一次重建第二個自動接手，不需要特別處理。

### 7.4 生命週期規則

| 情境 | 行為 |
|---|---|
| 啟動 | List → 建表 → **`WaitForCacheSync` 完成後才開 listener** |
| 啟動時 API server 連不上 | **直接 crash**（CrashLoopBackOff）。不要用空表硬跑——那會變成「pod Running 但全部拒絕」，最難查 |
| 執行中 API server 掛掉 | 繼續用本地 cache 服務。既有與新連線都正常。恢復後自動重新同步。發告警 |
| Pooler 被刪 / annotation 被移除 | 路由消失，**新連線**被拒。**既有連線完全不動**，等後端消失後自然收到 EOF |
| Pooler 新增 | 幾毫秒內生效，不需重啟 |

**路由變動永遠不會主動砍連線。** 主動砍沒有好處（後端都不在了，那些連線本來就要死），
卻讓「informer 抖一下」或「手殘 `kubectl annotate --overwrite`」變成整個租戶瞬間斷線。
這與第二階段 §8 的原則一致：出錯時的正確行為是什麼都不做。

### 7.5 RBAC

```yaml
kind: Role                    # namespace 範圍即可（測試環境 Pooler 都在同一個 ns）
rules:
  - apiGroups: ["postgresql.cnpg.io"]
    resources: ["poolers"]
    verbs: ["get", "list", "watch"]     # 唯讀
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create"]                   # 僅為發出撞名 / 格式錯誤事件
```

`list` 與 `watch` 都必須有——informer 的第一個動作是 List。
憑證由 k8s 自動掛載到 `/var/run/secrets/kubernetes.io/serviceaccount/`，
`rest.InClusterConfig()` 自動讀取，**程式裡不會有任何 credential**。

`setup.sh` 要包含前置檢查（RBAC 設錯的症狀是靜默的空路由表）：

```bash
kubectl auth can-i watch poolers.postgresql.cnpg.io \
  --as=system:serviceaccount:pgproxy-e2e:pg-proxy -n pgproxy-e2e
```

## 8. 設定與熱重載

### 8.1 設定項

```yaml
listen: ":5432"                    # 需重啟
metricsListen: ":9090"             # 需重啟

tls:
  certFile: /etc/pgproxy/tls/tls.crt
  keyFile:  /etc/pgproxy/tls/tls.key
  reloadInterval: 30s

backend:
  # mode 已於實作中移除：標準 TLS 交涉是唯一合法值，
  # 保留一個只有一種取值的設定項只會讓人以為它可以改。
  caFile: /etc/pgproxy/ca/ca.crt   # CNPG 內部 CA

proxyProtocol:
  enabled: false                   # 見 §9.4c，預設關閉
  trustedCIDRs: []                 # 允許送 PROXY header 的來源。
                                   # 空清單 = 誰都不信任、一律拒絕（fail-closed）。
                                   # enabled: true 但清單為空，在**設定載入時**就直接拒絕
                                   # 這份設定（不是安靜放行、之後才在執行期擋掉 100% 連線）。

route:
  namespace:          pgproxy-e2e
  hostnameAnnotation: pg-proxy.internal/hostname
  clusterAnnotation:  pg-proxy.internal/cluster

limits:                            # 以下皆可熱改
  maxConns:           5000         # ⚠ per-pod，3 replica = 全域 15000
  maxConnsPerCluster:  500         # 後盾，應高於 Pooler 的 max_client_conn
  cancel:                          # CancelRequest 濫用防護（design §6.1、§14）；
                                   # CancelRequest 本身不掛 Authorizer/Registry/連線上限，
                                   # 但這三個數字仍限制它能耗用多少 backend TLS dial。
    window:      10s
    maxPerIP:    5                 # 只在 proxyProtocol.enabled 時才實際生效——
                                   # 預設拓撲下所有連線共用 APISIX 的 pod IP，
                                   # 沒開 PROXY protocol 時這個數字會被視為 0（停用），
                                   # 否則「每 IP 5 次」實際上會變成「整個機群共用 5 次」。
    maxInFlight: 50                # 全域上限，不受拓撲影響，恆定生效

timeouts:
  handshake:   10s
  backendDial:  5s
  idle:       300s                 # 應短於雲端 LB 的 idle timeout

shutdown:
  drainTimeout: 300s               # 必須小於 terminationGracePeriodSeconds
```

測試環境覆寫：`maxConns: 20` / `maxConnsPerCluster: 10`，
否則在 8GB 的機器上根本測不到上限——**測不到的上限等於沒測**。

### 8.2 熱重載機制

憑證與設定共用一套：**每 `reloadInterval` 重讀檔案 → 比對 SHA-256 → 有變才 `atomic.Pointer` 換掉**。

不使用 fsnotify。k8s 更新掛載的 Secret/ConfigMap 是**建立新的 `..data` 目錄再原子換 symlink**，
在 `tls.crt` 上掛 watch 收不到 WRITE 事件，必須監看整個目錄並處理 CREATE/RENAME。
寫錯是靜默失效。定期重讀約 30 行、沒有邊界情況，而 kubelet 同步本來就有約 60 秒延遲，
fsnotify 省下的延遲在這個尺度沒有意義。

憑證透過 `tls.Config.GetCertificate` 生效——**這個 callback 每次握手都會被呼叫**：

```go
tlsCfg := &tls.Config{
    MinVersion: tls.VersionTLS12,
    GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
        c := store.cert.Load()
        if c == nil { return nil, errNoCert }
        return c, nil
    },
}
```

**既有連線不受影響**（它們早已完成握手），只有新連線用新憑證。這是預期行為，要寫成測試。

### 8.3 掛載限制

> **⚠ Secret 與 ConfigMap 一律不可用 `subPath` 掛載。**
> `subPath` 掛進來的檔案是 mount 當下複製一份，**之後永遠不會更新**，
> 定期重讀會永遠讀到舊內容，熱重載靜默失效。必須整個掛成目錄。

此限制要寫進 manifest 的註解裡。

## 9. 周邊五項

### 9.1 Graceful shutdown

```
SIGTERM → 關閉 listener（停收新連線）→ pgproxy_draining = 1
        → 等待 Registry 清空，或 shutdown.drainTimeout 到期
        → 強制關閉剩餘連線 → 退出
```

`terminationGracePeriodSeconds` 必須大於 `drainTimeout`，否則 kubelet 會在等待期間
直接 `SIGKILL`，這一項一定失敗。三個 replica 滾動更新時一個一個進行。

### 9.2 憑證/設定熱重載

見 §8.2。

### 9.3 連線上限與 nofile

全域與 per-cluster 上限共用 Registry 的計數，超限回 `53300`。

**啟動時把 nofile soft limit 拉到 hard limit：**

```go
var lim syscall.Rlimit
syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
lim.Cur = lim.Max
syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
```

k8s 的 pod spec **沒有設定 nofile 的欄位**，實際值由容器 runtime 預設決定，
各叢集不同（觀測值：OrbStack soft=20480 / hard=1048576；有些環境 soft 只有 1024）。
同一份 YAML 在不同叢集有不同上限而且看不出來，所以必須在程式裡自己拉。
拉完的值要 log 出來並曝成 metric——撞到上限時症狀是 `accept: too many open files`，
而服務仍顯示健康（既有連線正常、health check 通過、只有新連線建不起來）。

每條連線成本約 50–80KB（2 個 socket + 2 個 goroutine + TLS record buffer + relay buffer）。

### 9.4 後端故障與 idle timeout

- 撥不到後端 / 後端握手失敗 → `08006` ErrorResponse
- idle timeout：雙向都無流量超過 `timeouts.idle` 就關閉，`reason: idle_timeout`

> **已知限制**：idle timeout 的正確值取決於與 APISIX 及雲端 LB 對齊（proxy 的必須較短），
> 本機環境沒有雲端 LB，**只能驗證機制有效、無法驗證數字正確**。

### 9.4b Health / readiness probe

`:9090` 上除了 `/metrics` 另外提供兩個端點：

| 端點 | 語意 |
|---|---|
| `/healthz` | 行程活著就回 200。**不檢查 informer**——informer 卡住不該觸發重啟，重啟只會更糟 |
| `/readyz` | 「這個 pod 現在可以收新連線嗎」 |

`/readyz` 回 503 的兩種情況：

1. **informer cache 尚未同步完成**（開機階段）——這與 §7.4 的「同步完才開 listener」是同一件事的兩面：listener 沒開所以連不上，readiness 為 false 所以 Service 也不會把流量導過來。
2. **正在 drain**（收到 SIGTERM 之後）——讓 Service 盡快把這個 pod 從 endpoints 移除，新連線改導到其他 replica，既有連線則繼續 drain 到結束。

**probe 只掛在 `/readyz`（readinessProbe）與 `/healthz`（livenessProbe），
不要對 :5432 做 TCP probe**——那會在 pg-proxy 的 log 產生大量「連上就斷、沒有 SSLRequest」的雜訊。

### 9.4c PROXY protocol（實測結果：APISIX 能送，已驗證可用）

pg-proxy 實作 PROXY protocol **v1 與 v2 的解析**，放在設定項後面，**預設關閉**：

```yaml
proxyProtocol:
  enabled: false        # 開啟後，首包必須是 PROXY header，否則拒絕
```

開啟時的流程：accept 之後、讀 8 bytes 判斷首包型別**之前**，先解析 PROXY header 取得真實 client IP。

**實測結果（2026-09-09，apisix helm chart 2.17.0 / APISIX 3.18.0）：APISIX 的
stream route 對 upstream 能送 PROXY protocol，且 pg-proxy 能正確解析。**

設定方式：

```yaml
# apisix helm values（deploy/apisix-values.yaml）
apisix:
  proxyProtocol:
    enableTcpPPToUpstream: true   # 對 service.stream.tcp 的每個 port 都生效，非per-route
```

搭配 pg-proxy 這端：

```yaml
proxyProtocol:
  enabled: true
  trustedCIDRs: ["<pod CIDR，例如 192.168.194.0/24>"]   # 必須信任 APISIX 的 pod IP，否則會被 isTrustedProxySource 擋下
```

驗證方式：測試 client pod（IP `192.168.194.30`）經 APISIX 連線後，pg-proxy 的
`connect` 稽核事件記錄 `"client_ip":"192.168.194.30"`——與 APISIX 自己的 pod
IP（`192.168.194.34`）不同，證明收到的確實是真實來源，不是 APISIX 的轉發位址。

注意事項：
- `enableTcpPPToUpstream` 是**整個 stream 子系統**的開關，對 `service.stream.tcp`
  底下的**每一個 port**都生效，不是每條 route 各自設定。這份設計只有一條
  stream route（:5432），所以沒有影響，但多 route 的部署要留意。
- `proxyProtocol.trustedCIDRs` 必須涵蓋 APISIX gateway pod 實際的來源 IP
  （測試環境是整個 pod CIDR；正式環境建議收斂到 APISIX 所在的 node/pod 網段）。
- 這個設定目前**不是熱重載**的一部分（`main.go` 的 `buildHandler` 在啟動時把
  `cfg.ProxyProtocol.Enabled`/`TrustedProxyNets` 讀成固定值，不像
  `limits`/`timeouts`/cancel 上限是每次呼叫都重讀）——修改
  `proxyProtocol.*` 需要重啟 pg-proxy 才會生效，這點目前的 §8 開頭
  "listen/metricsListen/TLS 路徑需要重啟，其餘皆可熱改" 的敘述遺漏了，
  之後有時間應該把它也接上熱重載或者把文件改成明確排除它。

### 9.5 Metrics

`/metrics` 掛在**獨立的 :9090**，不可與 :5432 混用（那個 port 跑的是 PostgreSQL 協定）。

```
pgproxy_connections_active{cluster}              Gauge
pgproxy_connections_total{cluster,result}        Counter    # ok/denied/limit_exceeded/backend_error
pgproxy_handshake_errors_total{reason}           Counter    # unknown_sni/tls_error/bad_startup/plaintext_rejected/
                                                             # untrusted_proxy_source/bad_proxy_header/cancel_rate_limited
pgproxy_backend_dial_seconds{cluster}            Histogram
pgproxy_bytes_total{cluster,direction}           Counter
pgproxy_cert_expiry_seconds                      Gauge
pgproxy_draining                                 Gauge
pgproxy_nofile_limit                             Gauge
pgproxy_route_table_entries                      Gauge
pgproxy_route_table_last_sync_seconds            Gauge
```

**Cardinality 紀律：label 只用 `cluster`，絕不用 hostname 或 user。**
帶 user 的話每個新帳號都會生出一條新的時間序列。

`pgproxy_route_table_last_sync_seconds` 是「informer 靜默卡死」唯一的偵測手段——
數字持續增長代表 watch 斷了而沒重連。正式環境應對它設告警（例如 > 300s）。

接入方式：**PodMonitor CR**（`monitoring.coreos.com/v1`）。

## 10. 驗收矩陣

### 10.1 單元測試

| 對象 | 內容 |
|---|---|
| `pgwire` 首包判斷 | 四種型別 + 長度不足 + 未知 code |
| `pgwire` StartupMessage | 正常、缺 database（預設為 user）、格式錯誤、超長 |
| `pgwire` ErrorResponse | 位元組完全符合 §6.3，長度欄位正確 |
| `route` rebuild | 撞名拒絕、多層 hostname 拒絕、無 annotation 略過、刪除後移除、撞名者在前者刪除後接手 |
| `route` 併發 | `-race` 下同時 rebuild 與 Lookup |
| `registry` | 上限（全域/per-cluster）、Snapshot、Remove 後計數歸零 |
| `config`/`tlsutil` | hash 未變不換、變了才換、壞檔案不覆蓋既有值 |

### 10.2 端到端測試

| # | 情境 | 斷言 |
|---|---|---|
| 1 | `verify-full` 連 tenant1.db.test | 成功；`\conninfo` 顯示 TLS；connect 事件含正確 user/database/cluster |
| 2 | `verify-full` 連 tenant2.db.test | 成功且落到**不同**後端 → SNI 分流有效 |
| 3 | rootcert 換成 rogue CA | **必須失敗** → 證明不是偽裝的 `sslmode=require` |
| 4 | 未知 SNI | 明確拒絕不 hang；`handshake_errors_total{reason="unknown_sni"}` +1 |
| 5 | 開超過 `maxConnsPerCluster` | 收到 `53300`，訊息清楚 |
| 6 | apply 憑證 v2 | 新連線用 v2；**既有連線不受影響** |
| 7 | 新增一個帶 annotation 的 Pooler | 不重啟即可連上；`route_table_entries` +1 |
| 8 | 刪除該 Pooler | 新連線被拒；既有連線仍存活 |
| 9 | `kubectl rollout restart` | 既有長連線不中斷 |
| 10 | 長查詢按 Ctrl-C（跨 replica） | 查詢真的被取消 |
| 11 | metrics | 開 3 條 → `active`=3；全關 → **回到 0**（抓連線洩漏） |
| 12 | `sslmode=disable` | 收到明確的 ErrorResponse，不是靜默斷線 |

第 3 與第 11 是最有價值的兩項：前者證明 TLS 測試不是假的，
後者證明接縫 C 的登記表沒有洩漏（第二階段的撤銷掃描完全依賴它）。

### 10.3 憑證產生

`scripts/gen-certs.sh` 產出（**產物不進 git，只有腳本進**）：

| 產物 | 用途 |
|---|---|
| `testca.crt` / `.key` | 測試 root CA（模擬公司 CA） |
| `wildcard-v1.crt` / `.key` | `*.db.test`，pg-proxy 的對外憑證 |
| `wildcard-v2.crt` / `.key` | 同上但不同序號，測熱重載 |
| `rogueca.crt` + 其簽發的憑證 | 負向測試 |

**憑證必須有 `subjectAltName = DNS:*.db.test`。** 現代 libpq/OpenSSL 不看 CN，
只設 CN 會讓 `verify-full` 直接失敗——這是自簽憑證最常見的翻車點。

### 10.4 client 如何解析 hostname

測試 client pod 用 `hostAliases` 把 `tenant1.db.test` / `tenant2.db.test`
指向 APISIX gateway Service 的 ClusterIP（Service 指定固定 ClusterIP）。
CA 用 ConfigMap 掛載給 `PGSSLROOTCERT`（CA 憑證是公開資訊）。

## 11. k8s 交付物

全部放在新的 `pgproxy-e2e` namespace，**不動既有的 `cnpg-system` 與 `training`**。

```
CNPG Cluster × 2      tenant1-db / tenant2-db（各 1 instance，節省資源）
Pooler   × 2          tenant1-pooler / tenant2-pooler（帶 annotation）
pg-proxy              Deployment(replicas: 3) / Service / ConfigMap / Secret
                      PDB(minAvailable: 2) / ServiceAccount / Role / RoleBinding
                      PodMonitor
APISIX                helm 安裝 ingress controller，啟用 stream_proxy
                      一條 L4 stream route → pg-proxy Service:5432
測試 client           psql pod（hostAliases + CA ConfigMap）
```

節點資源是 8 CPU / 8GB 且已有其他工作負載，所以 CNPG Cluster 用單 instance 節省資源。

**pg-proxy 固定 3 replica，測試環境也不縮減。** 原因是 PDB 設 `minAvailable: 2`，
若 replica 只有 2，PDB 會禁止任何自願性中斷，滾動更新會直接卡死（第 9 項驗收永遠過不了）。
pg-proxy 本身是位元組轉發，resource request 很小（50m CPU / 64Mi），3 個 replica 不構成負擔。

腳本：

```
scripts/gen-certs.sh    產憑證
scripts/setup.sh        安裝全部（含 RBAC 前置檢查），可重複執行
scripts/e2e.sh          跑 §10.2 全部情境
scripts/teardown.sh     刪除 namespace + helm uninstall apisix
```

## 12. 已知限制

以下四項**明確記錄，不假裝解決**：

1. ~~來源 IP 可能被改寫~~ **已實測解除**：APISIX（chart 2.17.0 / APISIX
   3.18.0）的 stream route 可以對 upstream 送 PROXY protocol
   （`apisix.proxyProtocol.enableTcpPPToUpstream: true`），pg-proxy 開啟
   `proxyProtocol.enabled` 並信任 APISIX 的來源 CIDR 後，`client_ip`
   正確記錄真實來源，見 §9.4c。**唯一殘留限制**：這個設定不是熱重載的
   一部分，改動需要重啟 pg-proxy（見 §9.4c 注意事項）。
   另外 PgBouncer 本身不支援 PROXY protocol，所以 PostgreSQL 自己的
   `pg_stat_activity.client_addr` 看到的一定是 pooler pod IP，那一段仍然
   只能靠 `conn_id` 串起 pg-proxy 稽核 log 與 PostgreSQL 自己的紀錄——這一項
   無法靠自建解決，與 PROXY protocol 是否可用無關。
2. **idle timeout 對齊在本機無法完整驗證**（見 §9.4）。
3. **SCRAM channel binding 不能使用（已實測確認）。** TLS 在 proxy 終止、對後端重建，
   兩段的綁定值不同，client 設 `channel_binding=require` 會失敗。
   實測（psql 17.11，經 pg-proxy + APISIX）：
   `channel_binding=require` → `psql: error: ... channel binding is required,
   but server did not offer an authentication method that supports channel
   binding`；`channel_binding=prefer`（libpq 預設值）→ 連線成功。
   上線前要確認沒有團隊把 client 設成 `require`。
4. **Ctrl-C（CancelRequest）在 e2e 環境中實測不會取消查詢——這是本輪 e2e
   （§10.2 情境 10）新發現的限制，不是先前已知的三項之一。** 直接連
   PgBouncer（含 TLS）或直接連 PostgreSQL 都能正常取消（<1s 內查詢中止），
   但透過 pg-proxy 時，原查詢跑滿全長，且 `pgproxy_handshake_errors_total
   {reason="unknown_sni"}` 每次都同步 +1、沒有任何 `"event":"cancel"`
   稽核紀錄——與 APISIX 或 TLS 本身無關（繞過兩者直接打單一 pod 結果相同）。
   直接根因：pg-proxy 目前把 CancelRequest 的路由完全綁在 TLS SNI 上
   （design §6.1、§7），但實測顯示 psql 17 / libpq 17 的取消連線
   （`PQcancelBlocking`）建立 TLS 時**不會**重送原連線的 SNI，導致
   `Router.Lookup("")` 找不到路由，`handleCancel` 落入「未知 SNI」分支
   靜默丟棄。
   **這不是協定層的死路，只是本階段沒做**：後端在 startup 送的
   `BackendKeyData`（`'K'` 訊息）本身就帶著 pid + secret，`authOkWatcher`
   已經在監看同一條位元流，順手記下 `(pid, secret) → cluster` 的映射，
   cancel 就完全不需要依賴 SNI 路由——PgBouncer 與 pgcat 都是這樣實作的。
   真正的難點在**多副本**：cancel 連線經 APISIX 會被隨機分到某個
   pg-proxy replica，只有原本持有那條連線的 replica 才有這筆映射，其餘
   replica 收到會找不到對應的 pid+secret。要解需要跨 replica 的共享狀態
   （例如把映射存進一個所有 replica 都能查的地方）或廣播機制（對所有
   replica 送一次 cancel，各自查自己的本地映射），兩者都超出本階段
   單一 pod 內部就能完成的範圍，留給第二階段評估設計。**現況：跨
   replica 的長查詢 Ctrl-C 在本代理架構下不可靠，等同不可用，這比原本
   假設的「會生效」嚴重，上線前必須讓使用者知情。**（另見 README「已知
   限制」一節與 `internal/server/backend.go` 的 `verifyChainOnly` 旁的
   後續改進建議。）

## 13. log 格式

stdout，一行一筆 JSON，同一個 `conn_id` 串起整條連線。

```json
{"ts":"2026-09-09T10:58:03.220Z","event":"connect","conn_id":"01K5R7Q2XJ",
 "cluster":"tenant1","sni":"tenant1.db.test","client_ip":"10.42.7.19",
 "user":"analyst_ro","database":"orders","application_name":"metabase",
 "backend":"tenant1-pooler.pgproxy-e2e.svc:5432","tls":"TLSv1.3","handshake_ms":3.1}

{"ts":"2026-09-09T11:04:19.006Z","event":"disconnect","conn_id":"01K5R7Q2XJ",
 "reason":"client_close","bytes_in":18422,"bytes_out":9931204,"duration_s":375.8}
```

`reason` 值域：`client_close` / `backend_close` / `backend_error` / `idle_timeout` /
`shutdown` / `limit_exceeded`（第二階段增加 `policy_revoked`）。

這份 log 是第一階段最有價值的產物——第二階段要對帳的「實際上有哪些帳號在連哪個資料庫」
就靠它。第一階段必須確保**每一條連線都有非空的 `user` 與 `database`**。

## 14. 為第二階段預留

第一階段結束時，以下必須已經就位（否則第二階段是重寫）：

- 接縫 B 的呼叫點在流程第 6 步（解析後、撥後端前）
- `Route.Cluster` 已經帶著 cluster 識別字
- `Registry` 存得下每條連線的完整中繼資料並可列舉，且有 `Revoked` 欄位與兩側 socket
- `ErrorResponse` 已可產生任意 SQLSTATE
- 接縫 D 的 `io.Copy` 收在介面後面

### 14.1 四個接縫的現況與待補之處（e2e 驗收後補記）

| 接縫 | 現況 | 第二階段待補 |
|---|---|---|
| A · Router | `internal/route`：dynamic informer 讀 Pooler CRD，`Lookup(sni)`，實測對新增/刪除 Pooler 都在數秒內生效、不需重啟（e2e 情境 7/8） | 無待補——第二階段不改動路由來源 |
| B · Authorizer | `internal/authz`：介面已就位，第一階段用 `AllowAll` | 第二階段換成真正的政策查詢（中央服務 DB 經 Redis 投影，見 `pg-proxy-authz-audit.html`）；呼叫點與時機（StartupMessage 解析後、撥後端前）已固定，不需改動 `Handler` 其餘流程 |
| C · Registry | `internal/registry`：`TryAdmit`/`Add`/`Release`/`Remove`/`Snapshot`，每筆連線存 `ConnID/Cluster/SNI/ClientIP/User/Database/ApplicationName/Backend/StartedAt/Revoked` 與兩側 socket。**`Revoked` 欄位與 `Snapshot()` 目前完全沒有生產呼叫端**——這是第二階段撤銷掃描要接上的地方 | 撤銷掃描本身（週期性呼叫 `Snapshot()`、比對政策、對命中的連線設 `Revoked=true` 並主動關閉兩側 socket）；registry 的資料結構已經足夠，不需要改 schema |
| D · Relay | `internal/relay`：`io.Copy` 收在 `Relay` 介面後，雙向 idle timeout 已實作 | 若第二階段要做 SQL 稽核（訊息解析），需要在 Relay 內插入一層可觀察 client→backend 的 query text（目前是純位元組轉發，不解析 payload）；這是新增一層，不是改動現有介面 |

### 14.2 CancelRequest／SNI-routing 對第二階段撤銷掃描的影響

本輪 e2e 測試（§10.2 情境 10）發現 Ctrl-C 在本代理架構下不可靠（見 §12
第 4 項已知限制）：libpq 的 CancelRequest 連線不重送原連線的 SNI，
`Router.Lookup("")` 找不到路由，取消請求被靜默丟棄。

這件事**與第二階段的撤銷掃描是同一個根本問題的兩種呈現**：撤銷掃描要主動
關閉某條連線時，是直接拿 `Registry.Snapshot()` 給的 `clientConn`/
`backendConn` socket 呼叫 `Close()`（design §5、§14 開頭已定案的作法），
**不經過 SNI 或 CancelRequest**，所以撤銷掃描本身不受這個限制影響。
但如果第二階段之後打算額外提供「使用者自己按 Ctrl-C 取消長查詢」這個功能
（不同於管理端撤銷），現在就要知道：這條路目前不通，且修法需要一個不依賴
SNI 的取消連線識別機制，屬於架構層級的決定，不是小補丁。

## References

- [x] 需求來源（主）：`pg-proxy-phases.html` §0–§6
- [x] 需求來源（架構與周邊）：`pg-proxy-architecture.html` §4–§6
- [x] 第二階段脈絡（僅供理解接縫用途，不實作）：`pg-proxy-authz-audit.html`
- [x] Mockup / 設計稿：（none — 純後端服務，無 UI）
- [x] Design tokens：（none — 無 UI）
- [x] API contract / schema：PostgreSQL wire protocol 3.0（§6.1–§6.3 已內含所需的位元組格式）
