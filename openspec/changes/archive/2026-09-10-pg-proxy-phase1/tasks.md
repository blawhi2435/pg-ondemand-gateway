## 1. 專案骨架

<!-- TDD skipped: 純粹的專案設定與目錄結構，無行為可測 -->

- [x] 1.1 `go mod init github.com/blawhi2435/pg-ondemand-gateway`，Go 1.23
- [x] 1.2 建立 §4 的 package 目錄骨架與 `cmd/pg-proxy/main.go` 進入點
- [x] 1.3 加入 `.gitignore`（憑證產物、`.devflow-state.json`、build 產出）
- [x] 1.4 加入 `Makefile`：`test` / `lint` / `build` / `image`

## 2. pgwire — 首包型別判斷

- [x] 2.1 為四種首包（SSLRequest / GSSENCRequest / CancelRequest / 明文 StartupMessage）
      與兩種錯誤情況（長度不足、未知 code）寫失敗測試
- [x] 2.2 實作最小可通過的首包判斷
- [x] 2.3 重構：把 magic number 收成具名常數

## 3. pgwire — StartupMessage 解析

- [x] 3.1 為正常解析、缺 `database`（預設為 `user`）、缺 `user`、格式錯誤、
      超長 payload 寫失敗測試
- [x] 3.2 實作解析
- [x] 3.3 重構

## 4. pgwire — ErrorResponse

- [x] 4.1 為 §6.3 的位元組格式寫失敗測試，逐 byte 斷言，並驗證 length 欄位
      （含自身、不含 type byte）
- [x] 4.2 實作產生器，涵蓋 `08006` / `53300` 兩個第一階段用到的 SQLSTATE，
      且介面能產生任意 SQLSTATE（第二階段需要 `28000` / `57P01`）
- [x] 4.3 重構

## 5. route — 路由表重建與驗證

- [x] 5.1 為 rebuild 寫失敗測試：帶 annotation 進表、無 annotation 略過、
      多層 hostname 拒絕、撞名拒絕後者、前者刪除後由後者接手
- [x] 5.2 為併發安全寫失敗測試（`-race` 下同時 rebuild 與 Lookup）
- [x] 5.3 實作 `atomic.Pointer` 整份替換的 rebuild 與無鎖 Lookup
- [x] 5.4 重構

## 6. route — informer 與生命週期

- [x] 6.1 為生命週期契約寫失敗測試（用 fake dynamic client）：
      同步完成前 ready 為 false、初始 List 失敗回錯誤、
      新增 Pooler 後表增加、刪除後表減少
- [x] 6.2 實作 dynamic informer 的 Router，包含 `WaitForCacheSync` 與
      `last_sync` 時間戳更新
- [x] 6.3 實作撞名與非法 hostname 的 k8s Event 發送
- [x] 6.4 重構

## 7. registry — 連線登記表與上限

- [x] 7.1 為 Add/Remove/Snapshot/Count、全域上限、per-cluster 上限、
      以及「全部移除後計數歸零」寫失敗測試
- [x] 7.2 為併發 Add/Remove 寫 `-race` 測試
- [x] 7.3 實作登記表（含 §5 接縫 C 的完整 `Conn` 欄位與 `Revoked`）
- [x] 7.4 重構

## 8. authz — 接縫 B 空實作

- [x] 8.1 為 `allowAll` 永遠回 allow 寫失敗測試
- [x] 8.2 實作介面與 `allowAll`
- [x] 8.3 重構

## 9. relay — 雙向轉發

- [x] 9.1 為「任一端關閉即結束」、位元組計數正確、idle timeout 觸發
      寫失敗測試（用 `net.Pipe`）
- [x] 9.2 實作介面後的 `io.Copy` 雙向轉發，buffer 走 `sync.Pool`
- [x] 9.3 重構

## 10. config — 設定載入與熱重載

- [x] 10.1 為「雜湊未變不替換」、「變了才替換」、「無效設定不覆蓋既有值」、
      「負數逾時被拒絕」寫失敗測試
- [x] 10.2 實作 §8.1 的設定結構、定期重讀與 `atomic.Pointer` 替換
- [x] 10.3 重構

## 11. tlsutil — 憑證熱重載

- [x] 11.1 為「新憑證生效」、「壞憑證不覆蓋既有值」、
      「`GetCertificate` 每次呼叫都取當前值」寫失敗測試
- [x] 11.2 實作憑證 store 與 `tls.Config.GetCertificate`
- [x] 11.3 實作 `cert_expiry_seconds` 所需的 NotAfter 讀取
- [x] 11.4 重構

## 12. audit — 稽核事件

- [x] 12.1 為 connect / disconnect / cancel 三種事件的欄位完整性寫失敗測試，
      特別斷言 `user` 與 `database` 非空、`reason` 在值域內
- [x] 12.2 實作 JSON sink（一行一筆到 stdout）
- [x] 12.3 重構

## 13. metrics 與 probe

- [x] 13.1 為 §9.5 全部指標的註冊、`active` 增減與歸零、
      `handshake_errors_total{reason}` 分類寫失敗測試
- [x] 13.2 為標籤基數紀律寫失敗測試（斷言不存在 user/database/client_ip/sni 標籤）
- [x] 13.3 為 `/healthz` 與 `/readyz` 語意寫失敗測試：
      informer 異常時 healthz 仍 200、未同步時 readyz 503、drain 時 readyz 503
- [x] 13.4 實作 metrics registry 與獨立 port 上的 HTTP server
- [x] 13.5 重構

## 14. nofile 自動提升

- [x] 14.1 為「soft 被提升到 hard」與「結果被記錄」寫失敗測試
- [x] 14.2 實作 `Getrlimit`/`Setrlimit` 與 `pgproxy_nofile_limit` 曝光
- [x] 14.3 重構

## 15. PROXY protocol 解析

- [x] 15.1 為 v1（文字格式）與 v2（二進位格式）的解析、
      以及「啟用時首包非 PROXY header 則拒絕」寫失敗測試
- [x] 15.2 實作解析，以設定項控制，預設關閉
- [x] 15.3 重構

## 16. 連線流程組裝與 graceful shutdown

- [x] 16.1 為 §6 的 11 步順序寫失敗測試，特別斷言
      Authorizer 在撥後端**之前**被呼叫、登記發生在 `AuthenticationOk` **之後**
- [x] 16.2 為 graceful shutdown 寫失敗測試：SIGTERM 後停收新連線、
      既有連線續存、全部結束即退出、逾時強制關閉
- [x] 16.3 實作 `handleConn` 完整流程與後端標準 TLS 交涉
- [x] 16.4 實作 CancelRequest 的獨立路徑（不登記、不呼叫 Authorizer、不套上限）
- [x] 16.5 實作 listener 生命週期、drain 與訊號處理
- [x] 16.6 重構

## 17. 容器映像

<!-- TDD skipped: 建置設定 -->

- [x] 17.1 撰寫多階段 Dockerfile（distroless 或 static base），輸出 linux/arm64
- [x] 17.2 確認映像能被 OrbStack 的 k8s 直接取用（本地 build，`imagePullPolicy: Never` 或本地 registry）

## 18. 憑證產生腳本

<!-- TDD skipped: 測試資料產生 -->

- [x] 18.1 撰寫 `scripts/gen-certs.sh`：測試 CA、`*.db.test` v1、v2、rogue CA
- [x] 18.2 確保憑證帶 `subjectAltName = DNS:*.db.test`（僅 CN 無法通過 `verify-full`）
- [x] 18.3 確認憑證產物已被 `.gitignore` 排除

## 19. k8s manifests

<!-- TDD skipped: 部署設定；行為由第 21 組的 e2e 驗證 -->

- [x] 19.1 namespace、ServiceAccount、Role、RoleBinding（唯讀 Pooler + create Event）
- [x] 19.2 pg-proxy Deployment(replicas: 3)：TLS Secret 與 ConfigMap
      **以目錄掛載、不使用 subPath**（加註解說明原因）、
      probe 指向 metrics port、`terminationGracePeriodSeconds > drainTimeout`
- [x] 19.3 Service（固定 ClusterIP 供 hostAliases 使用）、PDB(minAvailable: 2)、PodMonitor
- [x] 19.4 兩組 CNPG Cluster（各 1 instance）與 Pooler（帶兩個 annotation）
- [x] 19.5 測試環境 ConfigMap：上限設小到可觸發（`maxConns: 20` / `maxConnsPerCluster: 10`）

## 20. APISIX

<!-- TDD skipped: 部署設定 -->

- [x] 20.1 以 helm 安裝 APISIX Ingress Controller，啟用 `stream_proxy`
- [x] 20.2 建立單一 L4 stream route → pg-proxy Service:5432，**不設定 `sni`**
- [x] 20.3 實測 APISIX 對 upstream 能否傳送 PROXY protocol，記錄結果與版本

## 21. 安裝、驗收與拆除腳本

- [x] 21.1 撰寫 `scripts/setup.sh`（含 `kubectl auth can-i` 的 RBAC 前置檢查），可重複執行
- [x] 21.2 撰寫 `scripts/e2e.sh` 涵蓋 §10.2 的 12 項情境，逐項輸出 PASS/FAIL
- [x] 21.3 撰寫 `scripts/teardown.sh`：刪除 namespace + helm uninstall APISIX，
      不影響既有的 `cnpg-system` 與 `training`
- [x] 21.4 執行 setup → e2e → teardown → setup → e2e 完整循環，確認可重複安裝與驗收

## 22. 文件

<!-- TDD skipped: 文件 -->

- [x] 22.1 撰寫 `README` 的 pg-proxy 段落：如何建置、部署、跑 e2e、拆除
- [x] 22.2 記錄三項已知限制的實測結果（PROXY protocol、idle timeout 對齊、
      SCRAM channel binding），PROXY protocol 須寫明 APISIX 版本與設定方式
- [x] 22.3 記錄第二階段的接手點（四個接縫的現況與待補之處）

## 23. Node 6 回修 — Critical（必須全部修完才能進 Node 7）

- [x] 23.1 為「`Length` 小於 HeaderSize」「`Length` 為 0」「`Length` = 0x7FFFFFFF」
      三種首包寫失敗測試，斷言回 `bad_startup` 且行程不 panic、不做大量配置
- [x] 23.2 在 `pgwire.Header.Kind()` 內加入長度條件：
      `HeaderSize < Length <= HeaderSize+MaxStartupMessageLength` 才算 KindStartupMessage；
      `handler.go` 配置前另加防禦性檢查（該行的安全性目前依賴非本地不變量）
- [x] 23.3 為「連線存活超過 handshake timeout 後仍能雙向傳輸」寫失敗測試
- [x] 23.4 進 `Relay.Run` 前清除 client 的 deadline（`client.SetDeadline(time.Time{})`），
      對稱於 `backend.go:73` 已有的那一行
- [x] 23.5 為 graceful shutdown 寫失敗測試，**且必須用會被 cancel 的 ctx**
      （現有三個 listener 測試都傳 `context.Background()`，因此測不到真實路徑）：
      SIGTERM 後既有連線繼續傳輸、全部結束才退出、逾時才強制關閉
- [x] 23.6 讓 accept loop 與每條連線的 ctx 生命週期獨立於 signal ctx，
      只由 `Listener.Shutdown` 在 drainTimeout 後才 cancel
- [x] 23.7 為「後端回 ErrorResponse 後關閉、從不送 AuthenticationOk」寫失敗測試，
      斷言 `HandleConn` 在期限內返回、寫出 `reason: backend_close` 的 disconnect 事件、
      且 `pgproxy_connections_active` 回到 0
- [x] 23.8 `authOkWatcher` 在後端 EOF/error 時關閉一個 `Closed` channel，
      並加進 `relayAfterAuth` 的 select
- [x] 23.9 為「查詢等待時間超過 idleTimeout 的長查詢不被中斷」與
      「單向大量傳輸（COPY 型）不被中斷」寫失敗測試
- [x] 23.10 改成雙向共用一個閒置判定：任一方向有流量就重置，
      只有兩個方向都靜止超過 `idleTimeout` 才關閉

## 24. Node 6 回修 — 未接線的功能（宣告完成但實際沒有呼叫端）

- [x] 24.1 為「啟用 proxyProtocol 時 client_ip 取自 PROXY header」寫失敗測試
- [x] 24.2 依設計 §9.4c 把 `proxyproto` 接進 accept 之後、`ReadHeader` 之前，
      由 `cfg.ProxyProtocol.Enabled` 控制；並確認只信任受信任來源的 header
- [x] 24.3 為 `pgproxy_bytes_total` 與 `pgproxy_nofile_limit` 寫「實際跑一條連線後數值非零」的失敗測試
- [x] 24.4 在 `relayAfterAuth` 呼叫 `Metrics.AddBytes`；在 `main.go` 用 `RaiseNofileLimit`
      的回傳值呼叫 `SetNofileLimit`
- [x] 24.5 為「設定 `tls.reloadInterval: 5s` 後重載間隔確實是 5s」寫失敗測試
- [x] 24.6 讓 `tlsutil.Store` 接受設定值；移除或實作 `backend.mode`
      （目前被驗證後丟棄，等於設定會撒謊）
- [x] 24.7 為「audit 驗證失敗時有 log 與 metric，不是靜默丟棄」寫失敗測試
- [x] 24.8 處理 `Audit.Connect/Disconnect` 的回傳錯誤；把 relay 與 audit 重複定義的
      reason 值域收斂成單一來源

## 25. Node 6 回修 — Important（正確性與資源保護）

- [x] 25.1 為「最小 ConfigMap（只給必要欄位）仍有有效的逾時與上限」寫失敗測試
- [x] 25.2 `validate()` 拒絕 0 值或套用設計 §8.1 的預設值，
      使殘缺設定不會靜默關掉所有資源保護
- [x] 25.3 為「pre-auth 階段的連線計入上限」寫失敗測試
      （目前已完成 TLS、已撥後端、等待認證的連線不計入任何上限）
- [x] 25.4 讓上限涵蓋 pre-auth 連線；把 `overLimit` 與 `Registry.Add` 的重複規則
      收斂成 Registry 介面上的單一 `TryAdmit`，消除 seam 洩漏
- [x] 25.5 為「同一組 Pooler 重建多次，撞名的勝方穩定不變」寫失敗測試
- [x] 25.6 讓撞名解析具決定性（例如依 name/namespace 排序），
      避免重建之間把線上流量翻到另一個租戶
- [x] 25.7 為「Pooler 缺少 cluster annotation」與「SNI 大小寫不同」寫失敗測試
- [x] 25.8 缺 cluster annotation 的 Pooler 拒絕進表並發事件；SNI 比對改為大小寫不敏感（RFC 6066）
- [x] 25.9 為「暫時性 Accept 錯誤後仍持續接受連線；永久性錯誤讓 readyz 轉 503」寫失敗測試
- [x] 25.10 修正 accept loop 的錯誤處理，並讓 metrics HTTP server 設定讀寫逾時
- [x] 25.11 為 CancelRequest 加上速率限制與獨立的 in-flight 上限，並寫對應測試
      （目前無任何資源控制，且是 PgBouncer cancel key 的免費爆破中繼）
- [x] 25.12 `registry.NewConn` 改為接受具名結構而非 8 個連續 string 參數
      （目前任意兩個對調都能編譯、測試照過，稽核 log 會靜默歸錯 user/cluster）

## 26. Node 6 回修 — 測試品質（讓上述問題下次能被抓到）

- [x] 26.1 修正 `TestMetrics_ConnectionsActive_IncDecAndZero`：實際跑連線過 `HandleConn`
      再斷言 gauge 歸零，而不是對 gauge 直接加減
- [x] 26.2 補齊未覆蓋的錯誤路徑測試：`failBackend`、`registerConnection` 的 Add 失敗分支、
      `dialBackend` 握手失敗回 `08006`、`handleCancel` 的負向路徑、GSSENCRequest 路徑、
      `handshake_errors_total{reason="tls_error"}`、全域 `maxConns`
- [x] 26.3 補 `LastSyncSeconds` 的測試（informer 卡死的唯一偵測訊號，目前零測試）
- [x] 26.4 憑證輪替測試補上「輪替期間既有連線存活」的實際連線斷言
- [x] 26.5 移除 listener 測試中六處把 `time.Sleep` 當同步用的寫法，改用明確的同步機制
- [x] 26.6 audit 事件測試從「欄位存在」改為「欄位值正確」，
      使欄位對應錯誤（例如 user 與 database 對調）會被抓到

## 27. Node 6 第二輪回修 — Critical

- [x] 27.1 為 `Listener` 的 drain 競態寫失敗測試：需在 `-race -count=200` 以上重現
      （accept 追蹤成功、但 `wg.Add(1)` 尚未執行時 `Shutdown` 介入）
- [x] 27.2 把 `wg.Add(1)` 移進 `track()` 的鎖內，使「發布連線」與「計入 WaitGroup」
      對 `draining` 旗標而言是原子的；確認符合 sync.WaitGroup 的
      「計數為零時的正值 Add 必須 happens-before Wait」契約
- [x] 27.3 為「認證成功後立即關閉的短連線，必定產生 connect 事件」寫失敗測試，
      需重複數千次並斷言 connect 與 disconnect 數量相等（目前約 50% 遺失）
- [x] 27.4 移除 select 的隨機性：`Detected` 必須優先於 `Closed`
      （例如先做一次 non-blocking 的 `select { case <-Detected: default: }`，
      或改用單一狀態值而非兩個 channel）
- [x] 27.5 讓 `authOkWatcher` 正確處理 `(n > 0, err != nil)` 的 read：
      必須先掃描那 n 個 byte 再判定關閉，否則同一次 read 裡的 AuthenticationOk 會漏掉
- [x] 27.6 為四個 `Registry.Release` 呼叫點各寫一個測試，
      逐一驗證：刪掉任一個呼叫，對應測試必須變紅（變異測試自證）
- [x] 27.7 補齊上述測試，確保容量保留在每一條錯誤路徑都會被釋放

## 28. Node 6 第二輪回修 — Important

- [x] 28.1 為「同一來源 IP 的大量 cancel 不應誤擋」寫失敗測試，
      模擬本設計的實際拓撲（所有連線經 APISIX，共用同一個 pod IP）
- [x] 28.2 重新設計 cancel 的濫用防護：per-IP 在此拓撲下等於全域限制，
      應改為僅用全域 in-flight 上限，或在 PROXY protocol 生效時才啟用 per-IP；
      被拒絕的 cancel 必須有 metric 與稽核紀錄，不可靜默丟棄
- [x] 28.3 修正 `CancelRateLimiter.seen` 的無上限成長（加入時間基準的淘汰）
- [x] 28.4 為「`proxyProtocol.enabled: true` 但 `trustedCIDRs` 為空」寫失敗測試
- [x] 28.5 讓該組合在**啟動時的設定驗證**就失敗，而不是安靜地拒絕 100% 的連線；
      並為「來源不受信任」與「header 格式錯誤」分開 metric reason
- [x] 28.6 把 cancel 的三個上限移進 ConfigMap（與其他上限一致、可熱改），
      不再硬編碼於 `main.go`
- [x] 28.7 把 metric 的 label 值收斂成具名常數（沿用 `internal/reason` 的做法），
      消除 23 處字面字串
- [x] 28.8 修正 `TestListener_SignalCancellationAloneDoesNotKillLiveConnections`
      使其在缺陷存在時真的會失敗（目前無論如何都會過）
- [x] 28.9 修正測試中「起了 goroutine 但從未等待」的模式，
      特別是 `TestListener_Serve_RetriesOnTemporaryAcceptError` 會把
      `Serve` 與 `HandleConn` 的 goroutine 洩漏到後續每一個測試

## 29. 文件與一致性

- [x] 29.1 修正 `Registry` 的 doc comment：drain 實際走 `listener.conns` 而非 Registry，
      註解目前宣稱了它沒有的角色，會誤導第二階段實作撤銷掃描的人
- [x] 29.2 確認設計文件 §8.1 與程式碼的設定結構完全一致
      （orchestrator 已移除 `backend.mode`、補上 `proxyProtocol.trustedCIDRs`，
      若本輪再有設定變動需同步更新）

## 30. Node 6 第三輪回修（併入 19–22 組同一輪處理）

- [x] 30.1 為「後端 read 逾時後才送 AuthenticationOk」寫失敗測試：
      斷言連線仍被登記、有 connect 事件、`connections_active` 正確
- [x] 30.2 修正 `authOkWatcher.Read`：逾時類錯誤（`netErr.Timeout()`）不得視為關閉，
      因為上層 `relay.sharedIdleReader` 明確會重試該錯誤。
      只有真正的 EOF / 永久性錯誤才關閉 `Closed`
- [x] 30.3 為「`limits.cancel.maxPerIP` 或 `maxInFlight` 為負數」寫失敗測試
- [x] 30.4 `validate()` 拒絕負數的 cancel 設定
      （目前 `maxInFlight: -1` 會靜默關掉 cancel 全域上限，且此設定可熱改）
- [x] 30.5 為 `serveUntilShutdown` 寫測試，覆蓋 round 3 新增的邏輯
      （`cancelLimitsFrom` 的拓撲判斷）以及「signal 取消不會殺掉既有連線」這個不變量
      —— 不要恢復已刪除的 `listener_signal_test.go`，那個測試無法失敗且與正式程式脫節
- [x] 30.6 把 `netErr.Temporary()`（已 deprecated）換成明確的錯誤判斷

## 31. APISIX 改用 ingress controller 的宣告式資源（使用者指示）

<!-- TDD skipped: 部署設定；行為由第 21 組的 e2e 驗證 -->

- [x] 31.1 調查現行 apisix-ingress-controller 對 L4/TCP stream route 的支援介面
      （Gateway API `TCPRoute` vs 傳統 `ApisixRoute` 的 tcp stanza），
      記錄各自需要的 controller 版本與 chart 設定
- [x] 31.2 改用 **controller 管理的 k8s 資源**宣告那條 stream route，
      不再用 Admin API 命令式設定。route 仍必須是純 L4、**不可設 `sni`**
- [x] 31.3 更新 `deploy/apisix-values.yaml` 與 `scripts/setup.sh`，
      確保 setup 完全宣告式、可重複執行
- [x] 31.4 重跑 setup→e2e→teardown→setup→e2e 兩個完整循環，
      確認結果與先前一致（預期 11/12，第 10 項仍為已知的 CancelRequest 限制）
- [x] 31.5 若 Gateway API 路徑需要額外 CRD 或 controller 設定，一併寫進 setup.sh 與 README

## 32. 驗證 controller 2.x + APISIX standalone（無 etcd）能否承載 L4 stream route

<!-- TDD skipped: 環境驗證；結論本身就是產出，負面結果同樣有效 -->

- [x] 32.1 部署 APISIX 3.13+ 於 **standalone API-driven 模式**（無 etcd），
      並在靜態設定（helm values → config.yaml）中啟用 `stream_proxy` 監聽 5432。
      **先單獨確認 stream listener 有起來**（例如 pod 內 netstat / APISIX log），
      這一步與 controller 無關
- [x] 32.2 部署 apisix-ingress-controller 2.x，確認它能連上 standalone 的
      Standalone Admin API（`/apisix/admin/configs`）並完成一次同步
- [x] 32.3 用 Gateway API `TCPRoute`（必要時加 `L4RoutePolicy`）宣告那條 L4 route，
      觀察 controller 推送時 Admin API 的實際回應。
      若出現 issue #2665 描述的 400，記錄**完整的請求與回應內容**
- [x] 32.4 給出明確結論：此組合能否承載 L4 stream route。
      記錄 APISIX 版本、controller 版本、chart 版本、以及判定依據
- [x] 32.5 （不適用——32.4 判定不可行，前提「若可行」不成立；見 32.6）
- [x] 32.6 若不可行 → 保留現行 legacy 版本作為測試環境用，
      並在 README 寫明：正式環境（standalone）不適用，附上實測證據、
      issue #2665 連結、以及可行的替代方向（pg-proxy 直接掛 LoadBalancer）

## 33. APISIX 改用 file-driven standalone（對齊公司正式環境）

<!-- TDD skipped: 部署設定；行為由第 21 組的 e2e 驗證 -->

- [x] 33.1 重寫 `deploy/apisix-values.yaml`：
      `deployment.mode: standalone` + `role: traditional` +
      `role_traditional.config_provider: yaml`；
      stream route 寫在 `deployment.standalone.config`（含結尾必要的 `#END`）；
      listener 走 `gateway.stream.enabled: true` / `tcp: [5432]`
- [x] 33.2 移除 etcd 相依與 apisix-ingress-controller
      （這條路徑不再需要 controller，刪除 `deploy/09-apisix-route.yaml`
      與 `deploy/apisix-ingress-controller-values.yaml`）
- [x] 33.3 更新 `scripts/setup.sh` 與 `teardown.sh`：只裝 apisix 一個 helm release，
      teardown 一併移除 round 6 調查期間安裝的 CRD
      （`*.apisix.apache.org` 與 `*.gateway.networking.k8s.io`，
      開工前叢集確認沒有這些，是本次工作裝的）
- [x] 33.4 重跑 setup→e2e→teardown→setup→e2e 兩個完整循環，
      預期仍 11/12（第 10 項為已知的 CancelRequest 限制）。
      **數字若有變化須查明原因，不得當作雜訊**
- [x] 33.5 更新 README：改寫 APISIX 那一節，說明 file-driven standalone 的完整設定、
      `#END` 的硬性要求、以及 API-driven + controller 為何不適用（保留 round 6 的證據鏈）
