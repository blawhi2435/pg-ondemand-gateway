## ADDED Requirements

### Requirement: 首包型別判斷

pg-proxy SHALL 讀取新連線的前 8 bytes（兩個 big-endian int32：length 與 code），
並依 code 分派到四種處理路徑。任一種都不得以靜默關閉連線的方式回應。

#### Scenario: SSLRequest

- **WHEN** 收到 `length=8, code=80877103`
- **THEN** 回覆單一 byte `'S'`，隨後以 wildcard 憑證進行 TLS 握手

#### Scenario: GSSENCRequest

- **WHEN** 收到 `length=8, code=80877104`
- **THEN** 回覆單一 byte `'N'`，client 會自行退回一般流程並改送 SSLRequest

#### Scenario: CancelRequest

- **WHEN** 收到 `length=16, code=80877102`
- **THEN** 再讀 8 bytes 取得後端 pid 與 secret key，依 SNI 轉發給對應 Pooler，
  且 **NOT** 登記到連線登記表、**NOT** 呼叫 Authorizer、**NOT** 套用連線上限，
  並輸出一筆 `cancel` 稽核事件

#### Scenario: 明文 StartupMessage

- **WHEN** 收到 `length=N, code=196608`（client 使用 `sslmode=disable`）
- **THEN** 回覆 ErrorResponse 說明本服務只接受 TLS 連線，然後關閉連線

#### Scenario: 未知的首包

- **WHEN** code 不屬於上述四種，或前 8 bytes 在握手 deadline 內讀不滿
- **THEN** 關閉連線並將 `pgproxy_handshake_errors_total{reason="bad_startup"}` 加 1

### Requirement: StartupMessage 解析

pg-proxy SHALL 在 TLS 握手完成後解析 StartupMessage，取出 `user`、`database`、
`application_name`，且 SHALL NOT 依據解析結果做任何允許或拒絕的判斷。
解析後的 StartupMessage MUST 原樣轉送給後端。

#### Scenario: 正常的 StartupMessage

- **WHEN** payload 為 `"user\0app_rw\0database\0orders\0application_name\0metabase\0\0"`
- **THEN** 解析出 `user=app_rw`、`database=orders`、`application_name=metabase`，並放行

#### Scenario: 未提供 database

- **WHEN** payload 含 `user` 但不含 `database`
- **THEN** `database` 取值為 `user`（PostgreSQL 慣例）

#### Scenario: 缺少 user

- **WHEN** payload 不含 `user` 欄位
- **THEN** 回覆 ErrorResponse 並關閉連線，`handshake_errors_total{reason="bad_startup"}` 加 1

#### Scenario: 格式錯誤或長度異常

- **WHEN** payload 沒有以雙 `\0` 結束、鍵值不成對、或宣告長度超過上限
- **THEN** 回覆 ErrorResponse 並關閉連線，不得 panic

### Requirement: ErrorResponse 產生

pg-proxy SHALL 能產生符合 PostgreSQL wire protocol 的 ErrorResponse 訊息，
並 SHALL NOT 以靜默 RST 拒絕連線（那會讓 client 端變成難以診斷的逾時）。

訊息格式為：`'E'`、int32 length（含自身、不含 type byte）、
`'S' "FATAL" \0`、`'V' "FATAL" \0`、`'C' <SQLSTATE> \0`、`'M' <訊息> \0`、`\0`。

#### Scenario: 位元組格式正確

- **WHEN** 以 SQLSTATE `08006` 與訊息 `backend unavailable` 產生 ErrorResponse
- **THEN** 輸出的位元組完全符合上述格式，且 length 欄位等於「除 type byte 外的總長度」

#### Scenario: SQLSTATE 對應

- **WHEN** 發生後端連不上或後端握手失敗
- **THEN** 使用 `08006`

#### Scenario: 超過連線上限

- **WHEN** 全域或 per-cluster 連線數已達上限
- **THEN** 使用 `53300`

#### Scenario: 寫出後關閉

- **WHEN** ErrorResponse 已寫入 client socket
- **THEN** 立即關閉該連線，不等待 client 回應
