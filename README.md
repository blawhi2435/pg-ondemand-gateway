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
