# pg-ondemand-gateway

Cluster A 的應用服務經由 wildcard 網域連線到 Cluster B 的 CloudNativePG Pooler 的架構設計文件。

以瀏覽器開啟即可閱讀。

| 文件 | 內容 |
| --- | --- |
| [cross-cluster-pooler.html](cross-cluster-pooler.html) | 架構評估：為什麼 nginx 的 SNI 分流對 PostgreSQL 預設無效、兩個可行方案、nginx 與 CNPG 設定、上線後會遇到的問題 |
| [edge-option-decision.html](edge-option-decision.html) | 方案選型：TLS 直通（A）與協定感知代理（B）的逐項比較與建議 |
| [passthrough-implementation.html](passthrough-implementation.html) | 方案 A：架構、TLS 不在邊界終止的原因、P1–P15 需注意的問題、改用 Traefik 的差異 |
| [passthrough-config.html](passthrough-config.html) | 方案 A 的設定清單，依套用順序排列 |
| [pg-proxy-architecture.html](pg-proxy-architecture.html) | 自建 PostgreSQL 感知代理：代答 SSLRequest、依 SNI 直接轉發到各租戶 Pooler，client 端零改動 |
| [pg-proxy-phases.html](pg-proxy-phases.html) | 實作計畫：第一階段只做 TLS、分流、記錄與轉發，第二階段才加權限；四個接縫、兩階段的驗收與故障演練 |
| [pg-proxy-authz-audit.html](pg-proxy-authz-audit.html) | 單一叢集下在 pg-proxy 加上存取控制與 SQL 稽核：兩道閘門、政策從中央服務 DB 經 Redis 投影、訊息解析的範圍與代價、三個版本的切分 |

## 版本相依

- direct TLS 需 PostgreSQL 17+ 的 libpq
- PgBouncer 1.25+（CloudNativePG 1.26 / 1.27 起新建 Pooler 預設 1.25.1）
- 可設定的 PgBouncer 參數為白名單，依 CloudNativePG 版本而異

## pg-proxy（實作，第一階段）

`cmd/pg-proxy` 是 [pg-proxy-architecture.html](pg-proxy-architecture.html) /
[pg-proxy-phases.html](pg-proxy-phases.html) 的第一階段實作：一個
PostgreSQL wire-protocol 感知的 TLS 終止代理，依 SNI 把連線分流到各租戶的
CNPG Pooler，本身不做存取控制（第二階段的範圍）。詳細設計見
[docs/superpowers/specs/2026-09-09-pg-proxy-phase1-design.md](docs/superpowers/specs/2026-09-09-pg-proxy-phase1-design.md)。

> **APISIX 該怎麼設定？** 見 [result.md](result.md)——涵蓋 file-driven 與
> API-driven 兩種 standalone 模式各自的做法、哪一種組合不可用及其原因、
> 以及兩個會靜默失效的設定（`admin.enabled` 的雙重角色、`#END` 的硬性要求）。

### 建置

```bash
make test    # go test ./... -race
make lint    # go vet + gofmt -l
make build   # 產出 bin/pg-proxy（linux/arm64，靜態連結）
make image   # docker build -t pg-proxy:dev .
```

### 部署到一個 k8s 叢集（e2e 環境）

以下三支腳本假設：目前的 kubectl context 已指向目標叢集、該叢集已安裝
CloudNativePG operator（`cnpg-system`，不會被這些腳本改動）、本機有
`docker`/`helm`/`openssl`。

```bash
scripts/gen-certs.sh   # 產生測試用 TLS 材料（testca / wildcard-v1,v2 / rogueca），輸出到 ./certs（不進 git）
scripts/setup.sh       # 建置映像、部署兩組 CNPG Cluster+Pooler、pg-proxy（3 replica）、APISIX、測試 client pod
scripts/e2e.sh         # 跑 design §10.2 的 12 項情境，逐項輸出 PASS/FAIL
scripts/teardown.sh    # 刪除 pgproxy-e2e namespace + helm uninstall apisix；不觸碰其他既有 namespace
```

`setup.sh` 與 `teardown.sh` 都是可重複執行的（`kubectl apply`/`helm upgrade`
語意）；一輪 `setup.sh → e2e.sh → teardown.sh → setup.sh → e2e.sh` 完整循環
用來確認「可重複安裝與驗收」（design §10.2 的驗收前提）。

一切都部署在新建的 `pgproxy-e2e` namespace，加上單一個叢集層級的 helm
release（`apisix`）；**不會建立、修改、或刪除
`cnpg-system`/`training`/`booking-system` 這些既有 namespace 裡的任何東西**。

### APISIX：file-driven standalone（task 33，對齊公司正式環境）

公司的 APISIX 是 **standalone（無 etcd）**部署，用 helm + values.yaml 管理。
standalone 底下其實有兩種子模式，這個專案先後都實測過，**結論是只有
file-driven 這一種能承載 L4 stream route**：

| | file-driven（本專案採用） | API-driven（task 32 實測，不可行——見下一節） |
|---|---|---|
| 設定來源 | `deployment.standalone.config`（chart 轉成一份 `apisix.yaml`，APISIX 每秒輪詢這個檔案） | 純記憶體，靠 `PUT /apisix/admin/configs` 整份替換 |
| `stream_routes` | ✅ 支援 | ❌ task 32 實測撞到的洞 |
| 需要 controller | 不需要——route 直接寫在 helm values 裡，`helm upgrade` 就能改 | 需要 apisix-ingress-controller 才有意義（不然沒人會呼叫那個 API） |

`deploy/apisix-values.yaml` 的關鍵形狀：

```yaml
etcd:
  enabled: false
apisix:
  admin:
    enabled: false   # 見下方「一個容易忽略的開關」——這個沒關掉，file-driven 完全不會生效
  deployment:
    mode: standalone
    role: traditional
    role_traditional:
      config_provider: yaml
    standalone:
      config: |
        stream_routes:
          - server_port: 5432
            upstream:
              nodes:
                "pg-proxy.pgproxy-e2e.svc:5432": 1
              type: roundrobin
  stream:              # 這是 listener 靜態設定，跟上面的 route 是兩回事
    enabled: true
    tcp: [5432]
service:
  stream:
    enabled: true
    tcp: [5432]
```

**`#END` 是 APISIX 自己的檔案格式硬性要求**：`apisix.yaml` 解析器
（`config_yaml.lua`）只看檔案最後 10 bytes 找 `#END\s*$`，找不到就整份視為
「還沒寫完」直接放棄讀取，**不會報任何錯誤，就是安靜地當作沒這個檔案**。
這個專案用的 apisix helm chart（`templates/apisix-config-cm.yml`）在渲染
ConfigMap 時已經自動幫 `deployment.standalone.config` 的內容補上這一行，
所以上面這份 values 不需要手動加 `#END`——但如果哪天改成用
`existingConfigMap` 自己管理這個檔案，**一定要記得自己補上**，漏掉會讓
route 永遠讀不到、卻連個錯誤訊息都沒有，非常難查。

**一個容易忽略的開關**：`apisix.admin.enabled`（渲染成 config.yaml 的
`enable_admin`）**同時決定了 file-driven 還是 API-driven**，跟
`role_traditional.config_provider: yaml` 是兩個獨立但都要對的開關——
`internal/apisix/core/config_yaml.lua` 的 `is_use_admin_api()` 只看
`enable_admin`，如果是 `true`，config_yaml 模組會整個放棄讀取
`apisix.yaml`，改成等一個永遠不會來的 Admin API 推送。症狀是：容器
`1/1 Running`，但 `/status/ready` 永遠回 503，log 裡只有反覆的
`worker id: N has not received configuration`，**沒有任何 parse 錯誤或
其他線索**——這正是本輪一開始踩到的坑，花了不少時間才追到
`config_yaml.lua` 裡這一行判斷。**這份 values 必須明確設
`apisix.admin.enabled: false`**，chart 的預設值是 `true`。

### APISIX standalone（無 etcd）+ controller 2.x 實測結論（task 32）

> **範圍修正（task 34，見 [result.md](result.md)）**
>
> 本節原本的結論寫成「standalone 無法承載 L4 stream route」，**那個範圍太寬**。
> 後續實測證實：**API-driven standalone 直接 `PUT /apisix/admin/configs`
> 是可以承載 `stream_route` 的**（`apisix/admin/standalone.lua` 的 `patch_schema`
> 明確接受該型別，並以 psql `verify-full` 實連驗證通過）。
>
> 不可用的只有**「透過 apisix-ingress-controller 2.2.0 的 CRD 管理這條路由」**
> 這一條路徑——原因是它內建的 adc sync client 呼叫傳統的 per-resource 端點
> `/apisix/admin/routes`，而 standalone 只提供 `/apisix/admin/configs`。
> 那是 controller 的缺陷，不是模式的能力限制。
>
> 以下內容保留為 controller 那條路徑的完整證據鏈。

**結論：`controller 2.x + standalone` 這個組合無法承載 L4 stream route。
已用兩層獨立測試定位到確切原因，不是臆測。**

- **測試環境**：`apisix/apisix` chart 2.17.0（APISIX 3.18.0）、
  `apisix-ingress-controller` 2.2.0（同一個 chart 內建的 subchart，帶
  `adc` 0.27.1 sidecar）、standalone API-driven 模式
  （`deployment.role: traditional` + `role_traditional.config_provider:
  yaml`——見下方「一個容易選錯的設定」）。

- **32.1（stream listener，與 controller 無關）：通過。** 在完全不安裝
  controller 的情況下，純靠 helm values 把 `stream_proxy.tcp` 設進
  `config.yaml`，直接對 APISIX pod 的 5432 埠做 TCP connect 就成功。**這
  證實了原本的懷疑：靜態設定（stream listener）在 standalone 模式下完全
  正常，issue #2665 描述的問題不在這一層。**

- **32.2（controller 連上 Standalone Admin API）：部分通過。** controller
  能連上 `/apisix/admin/configs`（TCP/認證都沒問題），`GET` 該端點會回
  `200` 加上一份有效的 JSON（各資源的 `conf_version` 計數器）。但 controller
  自己的每一次週期性同步（無論有沒有先建 TCPRoute）都失敗。

- **32.3（用 TCPRoute 推送 route）：確認重現 issue #2665 描述的失敗，且
  找到比 issue 本身更精確的根因。** 完整證據鏈：

  1. controller 的 `manager` 容器持續（6+ 分鐘、每 30–60 秒一次，next
     GatewayClass/Gateway/TCPRoute 都建好、backend 也 resolve 成功之後
     仍然）記錄：
     ```
     ERROR provider.executor  failed to run http sync for server
       {"server": "http://apisix-standalone-admin.pgproxy-e2e.svc:9180",
        "error": "ServerAddr: ..., Err: HTTP 500:
          {\"message\":\"AxiosError: Request failed with status code 404\"}"}
     ```
     （issue #2665 回報的是 400；這裡實測到的是 404 被 adc sidecar 包成
     500——同一種「這條路徑走不通」的根本不相容，只是確切狀態碼不同，
     這本身也是一個值得記錄的澄清。）
  2. 直接對同一個 Admin API 用 `curl` 手動 `GET /apisix/admin/routes`
     （傳統、逐資源的舊式端點），拿到 APISIX/openresty **自己**回的
     `404 Not Found`——不是網路層或認證層的問題，是 APISIX 在這個模式下
     **根本沒有註冊**這個端點。而 `PUT /apisix/admin/configs`（新的整包
     推送端點）用同一把 admin key 手動 curl 完全正常（`202 Accepted`，
     `stream_routes_conf_version` 確實遞增）。
  3. 結論：**APISIX standalone（`config_provider: yaml`）模式只提供新的
     `/apisix/admin/configs` 整包端點，完全移除了傳統逐資源端點；而
     apisix-ingress-controller 2.2.0 / adc 0.27.1 的 APISIX 同步用戶端
     顯然還是在打舊式端點，兩者對不上。** `apisix_standalone-admin`
     service 上的 `stream_routes_conf_version` 全程停在我自己手動
     `curl` 留下的版本號，controller 一次都沒有成功推送過。

- **一個容易選錯的設定**：helm values 裡 `apisix.deployment.role` 有
  `data_plane`/`control_plane`/`traditional` 三個選項，直覺會選
  `data_plane`（畢竟這是「資料面」）。**這是錯的**——這個 chart 的
  `config.lua`／`ngx_tpl.lua` 只在 `role: traditional` 分支才會產生
  `lua_shared_dict standalone-config` 這個 nginx 層級的共享記憶體宣告；
  選 `data_plane` 的話這個宣告完全不會出現在 `nginx.conf`，任何打中
  Admin API 的請求都會直接 "Empty reply from server"（連 404 都不是，
  worker 內部直接崩掉），比後面的 404 更難排查。**正確組合是
  `role: traditional` + `role_traditional.config_provider: yaml`**——這
  才是「單一 instance 同時扮演 control plane 與 data plane」的
  API-driven standalone，`role: data_plane` 是給多節點分離部署（有另一個
  真正的 control plane 在別處推設定）用的，不適用於單機 standalone。這個
  區分**目前的 helm chart 文件/註解沒有講清楚**，值得記錄。

- **32.4 明確結論**：APISIX 3.18.0 + apisix-ingress-controller 2.2.0
  （含 adc 0.27.1）+ Gateway API `TCPRoute` 在 standalone API-driven 模式
  下**無法承載 L4 stream route**——不是設定錯誤，是 controller 端的 APISIX
  同步用戶端與 APISIX standalone 模式的端點介面不相容，與 issue #2665
  的結論一致（[apache/apisix-ingress-controller#2665](https://github.com/apache/apisix-ingress-controller/issues/2665)，
  2025-11 開，撰寫本文件時仍未解、無 maintainer 回應）。

- **32.6（round 6 當下不可行時的收尾，現況見上一節）**：round 6 完成時
  e2e 測試環境維持了 task 31 的組合（v1 controller、chart 0.14.1、app
  1.8.0、`apisix` chart 帶 etcd 的傳統模式），因為當時 standalone 這條路
  被判定不可行。**task 33 後來發現 round 6 只驗證了 standalone 的
  API-driven 子模式**，file-driven 子模式其實可行（見上一節）——現在
  e2e 測試環境已經改用 file-driven standalone，同時對齊了公司的正式環境
  拓撲，不再需要為了測試而跟正式環境用不同的路由管理機制。

由於 file-driven standalone 已確認可行且與正式環境一致，設計文件 §1
原本留的「pg-proxy 直接掛 LoadBalancer」伏筆目前不是必要選項——但仍值得
記錄：**APISIX 在這條路徑上看不到 SNI、不做任何分流決策、不終止 TLS，
全部連線走同一條 stream route**，它唯一的角色是把 TCP 位元組轉發到
pg-proxy Service。若之後有其他理由要拿掉這一層（例如想同時啟用 PROXY
protocol 又不想依賴 APISIX 的設定），跳過 APISIX、讓 pg-proxy 直接掛
`Service type: LoadBalancer` 仍是一個架構上乾淨的備案，留供之後評估。

### 已知限制

見設計文件 §12（四項，含本輪 e2e 新發現的 Ctrl-C 限制）以及
[docs/superpowers/specs/2026-09-09-pg-proxy-phase1-design.md](docs/superpowers/specs/2026-09-09-pg-proxy-phase1-design.md)
§14（第二階段的接手點）。

補充兩點使用者的修正/建議（本輪未實作，留給下一輪）：

- **Ctrl-C（CancelRequest）並非協定層不可能解決。** 後端在 startup 送的
  `BackendKeyData`（`'K'` 訊息）帶著 pid + secret，`authOkWatcher` 已經在
  監看同一條位元流，記下 `(pid, secret) → cluster` 的映射就能讓 cancel
  不依賴 SNI 路由——PgBouncer 與 pgcat 都是這樣做的。真正的難點是**多副本**：
  cancel 連線會被 APISIX 隨機分到某個 pg-proxy replica，只有原本持有那條
  連線的 replica 才有映射，其餘 replica 收到會找不到。需要跨 replica 的
  共享狀態或廣播機制才能解，不是單一 pod 內部就能修好。正確的說法是
  「需要 BackendKeyData 映射 + 跨副本共享狀態，本階段未實作」，不是
  「協定層限制、不可修」。
- **backend TLS 驗證可以做到完整 hostname 驗證，不必停在 verify-ca。**
  Pooler CR 的 `spec.cluster.name` 就是 CNPG Cluster 名稱，而 Cluster 的
  伺服器憑證 SAN 涵蓋 `<clusterName>-rw`。如果 route 表把這個名稱也存
  起來，`dialBackend` 呼叫時把 `BackendTLS.ServerName` 設成
  `<clusterName>-rw`，就能在不犧牲驗證強度的前提下解決 Pooler 與 Cluster
  主機名不一致的問題（目前的 `verify-ca` 折衷寫在
  `internal/server/backend.go` 的 `verifyChainOnly`）。
