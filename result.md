# APISIX standalone 能不能承載 PostgreSQL 的 L4 stream route

日期：2026-09-10 · 環境：OrbStack k8s v1.33.9 · APISIX 3.18.0（chart 2.17.0）

## 結論

**能。而且不需要改變現有的部署模式。**

先前一度得到「API-driven standalone 不能承載 stream route」的結論，**那是錯的**。
實際失敗的範圍比那窄得多：

| 組合 | stream route | 依據 |
|---|---|---|
| **file-driven standalone**（`apisix.yaml` 的 `stream_routes`） | ✅ 可用 | e2e 兩循環，11/12 |
| **API-driven standalone**（直接 `PUT /apisix/admin/configs`） | ✅ 可用 | 本文件 §2 實測 |
| API-driven standalone **+ apisix-ingress-controller 2.2.0 的 CRD** | ❌ 不可用 | §3，controller 缺陷 |

換句話說：**APISIX 本身兩種 standalone 模式都支援 L4 stream route。
不能用的是「透過 ingress controller 的 CRD 去管理它」這一條路徑。**

---

## 1. 原始碼依據

`apisix/admin/standalone.lua` 的 `patch_schema` 列出 Standalone Admin API 接受的資源型別：

```lua
"proto", "global_rule", "route",
"stream_route",          -- ← 一等公民，不是遺漏
"service", "upstream", "consumer",
"consumer_group", "credential", "ssl", "plugin_config",
```

`all_workers_applied()` 同時檢查 HTTP 與 stream 兩個子系統
（`stream_tracked and stream_tracked[key]`），代表 stream route 在這個模式下
有完整的套用追蹤，不是被順帶接受而已。

> 官方文件的資源型別清單（routes / upstreams / services / consumers / ssls / protos …）
> **沒有列出 `stream_routes`**，容易讓人以為不支援。以原始碼為準。

## 2. 實測（task 34）

部署一個獨立的 API-driven standalone 實例，**完全不安裝 ingress controller**，
直接對 Admin API 推送設定。

**設定形狀**

```yaml
apisix:
  admin:
    enabled: true                    # API-driven（與 file-driven 相反）
  deployment:
    role: traditional
    role_traditional:
      config_provider: yaml
gateway:
  stream:
    enabled: true                    # 靜態設定：L4 listener
    tcp:
      - 5432
```

**推送與回應**

```
PUT /apisix/admin/configs   （payload 帶 stream_routes 陣列，指向 pg-proxy）
  → 202 Accepted

GET /apisix/admin/configs
  → stream_routes_conf_version: 1，route 內容原樣回傳

GET /status/ready
  → 200 {"status":"ok"}       ← all_workers_applied() 確實追蹤到 stream 子系統
```

**真正的驗收 —— 不只看 API 回應**

`202` 與 `200` 都不足以證明流量會通。Round 6 的教訓正好相反：當時 controller
連得上 Admin API、`GET /configs` 也回 200，但同步從頭到尾沒有成功過。所以本次
以實際連線為準：

- `psql sslmode=verify-full` 連 `tenant1.db.test` → **成功**
- `psql sslmode=verify-full` 連 `tenant2.db.test` → **成功，落在不同的後端 IP**
  （pg-proxy 的 SNI 分流在底下正常運作）
- 換成不相干的 CA 當 rootcert → **仍然失敗**
  （確認不是憑證驗證被意外關掉造成的假陽性）

## 3. 那 round 6 到底測到了什麼

Round 6 測的是 **ingress controller 推送 TCPRoute**，那條路徑確實壞掉，
但根因與「模式的能力」無關：

- controller 2.2.0 內建的 `adc` 0.27.1 sync client 呼叫的是
  **傳統的 per-resource 端點 `/apisix/admin/routes`**
- standalone 模式**只提供 `PUT /apisix/admin/configs`**，前者根本不存在
- 手動 `curl GET /apisix/admin/routes` 得到 APISIX 自己發出的真 404，
  證實端點不存在
- 症狀是 adc 回 `HTTP 500: AxiosError 404`，
  `stream_routes_conf_version` 從未前進
- 對照 [apache/apisix-ingress-controller#2665](https://github.com/apache/apisix-ingress-controller/issues/2665)
  （2025-11 開，未解，無 maintainer 回應）。該 issue 回報的是 400，本次觀察到
  的是包成 500 的 404——同一個不相容，狀態碼不同

另外 round 6 也獨立證實：**在完全不裝 controller 的情況下，
純靠靜態設定就能把 stream listener 起起來、TCP 連得上。**
所以 issue #2665 說「stream port 需要改 config.yaml，但這個模式下 config.yaml
不被使用也改不了」**是錯的**——靜態設定完全正常。

### 這次推論錯在哪

Round 6 的觀察都正確，錯的是從觀察跨到結論的那一步：

> 觀察：controller 推 TCPRoute 進 API-driven standalone 失敗
> 錯誤結論：API-driven standalone 不能承載 stream route
> 正確結論：**這個 controller 版本的 sync client 與 standalone 的端點集不相容**

差別在於「管理介面」與「能力」是兩件事。記錄下來，是因為 round 6 的證據鏈本身
很有說服力，看到它的人很容易再得出同一個過寬的結論。

## 4. 兩個會靜默失效的設定

### 4.1 `apisix.admin.enabled` 有雙重角色

名字看起來只是「要不要開管理 API」，但 `apisix/core/config_yaml.lua` 拿它來決定
**設定要從哪裡讀**：

```lua
local function is_use_admin_api()
    local local_conf, _ = config_local.local_conf()
    return local_conf and local_conf.apisix and local_conf.apisix.enable_admin
end
```

在 `config_provider: yaml` 之下：

| `enable_admin` | 設定來源 | 行為 |
|---|---|---|
| `false` | 檔案 | `ngx.timer.every(1, read_apisix_config)` 每秒讀 `apisix.yaml`，版本用檔案 mtime |
| `true` | 記憶體 | **那個 timer 不會啟動**，改等 Admin API 推送 |

**失敗時完全沒有錯誤訊息。** 若打算用 file-driven 卻把 admin 留在預設的 `true`：
APISIX 不會去讀 `apisix.yaml`（不是讀失敗，是根本沒讀），路由表是空的，
`/status/ready` 永遠 503，而 pod 顯示 `1/1 Running`、error log 一行都沒有。

實務風險：在 file-driven 環境下，有人為了臨時查看設定把 Admin API 打開，
會讓所有路由消失，而症狀看起來像網路或 upstream 問題。

### 4.2 `#END` 是 YAML 格式的硬性要求

file-driven 的 `apisix.yaml` 結尾必須有 `#END`，否則整份設定**靜默不被讀取**——
失敗徵狀與 4.1 完全相同，兩個一起中會很難分辨。

- 用 helm values 的內嵌 `deployment.standalone.config` 時，chart 的
  `apisix-config-cm.yml` template 會自動補上
- 改用 `existingConfigMap` 時**必須自己加**

## 5. 對正式環境的建議

**兩種 standalone 模式都不需要為了這條路由而改變。** 依現況選對應做法：

### 現況是 file-driven（`apisix.yaml` / ConfigMap 管設定）

在既有設定加一段即可：

```yaml
stream_routes:
  - server_port: 5432
    upstream:
      nodes: {"pg-proxy.<namespace>.svc:5432": 1}
      type: roundrobin
#END
```

加上靜態的 listener（`gateway.stream.enabled: true` / `tcp: [5432]`）。
一秒內熱重載，不需重啟。

### 現況是 API-driven（自有自動化 / ADC 推送設定）

在既有的 `PUT /apisix/admin/configs` payload 加入 `stream_routes` 陣列即可，
**HTTP 路由的管理方式完全不受影響**。同樣需要靜態的 listener。

### 現況是 API-driven + ingress controller 管理路由

這一條 L4 route 無法透過 CRD 管理（§3）。三個選項：

1. **這條路由改用直接 PUT** —— 其餘仍由 controller 管理。
   注意 controller 每次全量同步會覆寫手動推送的設定，需確認共存方式
2. **pg-proxy 直接掛 `Service type: LoadBalancer`** —— 跳過 APISIX。
   APISIX 在這條路徑上本來就不做任何決策（看不到 SNI、不分流、不終止 TLS，
   stream route 只有一條），順帶也解決 PROXY protocol 的依賴
3. 等上游修 controller 的 sync client（issue #2665 目前無人回應）

## 6. 附帶確認：PROXY protocol 可用

`enableTcpPPToUpstream: true` 實測有效（chart 2.17.0 / APISIX 3.18.0），
pg-proxy 記錄的 `client_ip` 與真實 client pod IP 完全吻合。
設計文件原本將此列為未知數，實測結果為正面。

本次交付的預設值維持關閉，因為 e2e 其他情境會直連 pod（不經 APISIX），
開啟後那些情境需自行送 PROXY header。

## 版本

| | |
|---|---|
| APISIX | 3.18.0（chart `apisix/apisix` 2.17.0） |
| apisix-ingress-controller | 2.2.0（內建 adc 0.27.1） |
| Kubernetes | v1.33.9+orb1（OrbStack） |
| CloudNativePG | 已存在於叢集（`cnpg-system`） |
