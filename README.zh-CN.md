# BSC BNB 充值到账 Webhook 接收端

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[English](README.md) | 简体中文

**BNB Smart Chain 原生 BNB 充值到账**的 Webhook 接收端，Node.js、Python、Go 各一份，都是单文件、**零依赖**，可以直接跑。

只看 `tx.to` 的监听程序会漏掉通过合约转进来的 BNB：多签钱包（Safe）转出、批量打款合约、DEX 兑换成 BNB 直接打到收款地址。这类「内部转账」不产生日志，`eth_getLogs` 也查不到。[BSC Query](https://bscquery.com/bnb?utm_source=github&utm_medium=referral&utm_campaign=examples) 对每个区块做 trace，把监控地址的每一笔 BNB 变动推送给你，包括内部转账和 gas 手续费，并附带区块结束时的地址余额。

这几个接收端处理了接入时容易写错的地方：

- **验签**：对原始请求体做 HMAC-SHA256，拒绝时间戳相差 5 分钟以上的请求，防止伪造和重放。
- **只入账一次**：按变动 `id` 去重，只在 `finalized` 时入账。平台重试、重复推送、补漏拉到的旧数据都不会重复记账。
- **区块回滚**：`pending` 只显示「到账中」，`finalized` 入账，`removed` 撤销「到账中」。
- **整批成功才返回 200**：整个请求落盘后才返回 200；中途出错就丢弃这批改动并返回 500，等平台重试整批。
- **两种请求格式**：单区块，以及打开「多区块合并推送」后的 `{"batches": [...]}`。
- **补漏**：启动时用 REST 补拉最近 24 小时，之后每小时补拉最近 2 小时，服务宕机期间漏掉的推送会自动补齐。

## 快速开始

### 1. 开通免费档

用 BSC 钱包登录 [bscquery.com](https://bscquery.com/bnb?utm_source=github&utm_medium=referral&utm_campaign=examples)，开通 BNB 余额监控免费档（10 个地址，每 30 天 1 万条变动，只推已确认的 `finalized`）。在 BNB 套餐页拿到两样东西：

- **Token**：`bscg_bnb_` 开头，调 REST 接口用；
- **验签密钥**：在「Webhook 回调」区域。

### 2. 启动接收端

任选一种语言：

```bash
git clone https://github.com/calmw/bsc-bnb-deposit-webhook.git
cd bsc-bnb-deposit-webhook

export BSCG_WEBHOOK_SECRET='你的验签密钥'
export BSCG_BNB_TOKEN='bscg_bnb_...'   # 可选，不设就不补漏

cd node   && node server.mjs        # Node.js 18+
cd python && python3 server.py      # Python 3.9+
cd go     && go run .               # Go 1.22+
```

默认监听 `http://0.0.0.0:8080/bnb-webhook`。

| 环境变量 | 说明 | 默认值 |
|---|---|---|
| `BSCG_WEBHOOK_SECRET` | 验签密钥，必填 | — |
| `BSCG_BNB_TOKEN` | BNB Token，用于补漏 | 不设则不补漏 |
| `BSCG_API` | 接口地址 | `https://api.bscquery.com` |
| `PORT` | 监听端口 | `8080` |
| `STATE_FILE` | 状态文件 | `state.json` |
| `BACKFILL_HOURS` | 启动时补拉多少小时 | `24` |

### 3. 本地联调（不用真转账）

另开一个终端：

```bash
BSCG_WEBHOOK_SECRET='你的验签密钥' ./send-test.sh
```

脚本会签名并发送：一条测试推送、一笔充值从 `pending` 到 `finalized`、一次重复推送、一个合并推送格式的请求、一笔被区块回滚撤销的充值，以及两个伪造请求。正常输出：

```text
test-push -> HTTP 200
pending -> HTTP 200
finalized -> HTTP 200
duplicate -> HTTP 200
batches -> HTTP 200
reorg-pending -> HTTP 200
reorg-removed -> HTTP 200
bad-signature -> HTTP 401
stale-timestamp -> HTTP 401
```

接收端只入账一次 0.5 BNB、一次 1 BNB，被回滚的 2 BNB 不入账，`state.json` 里的余额是 1.5 BNB。

### 4. 接上真实推送

回调地址必须是公网 HTTPS。本机调试可以用 Cloudflare Tunnel 临时暴露：

```bash
cloudflared tunnel --url http://localhost:8080
# 回调地址填 https://xxxx.trycloudflare.com/bnb-webhook
```

在 BNB 套餐页填好回调地址，点「发送测试推送」，接收端会打印 `test push received`。

然后用接口添加要监控的地址：

```bash
curl -X POST https://api.bscquery.com/v1/bnb/addresses \
  -H "Authorization: Bearer $BSCG_BNB_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"addresses":["0x你的地址"]}'
```

往这个地址转 0.001 BNB，几秒后接收端会打印 `credited ...`。再从多签或 DEX 兑换往里转 BNB，会看到 `(internal)`。

## 一条变动怎么推送

```text
区块里有监控地址的变动
  → pending      显示「到账中」，不入账
  → finalized    入账一次（BSC 有快速最终性，通常几秒）
  → 或 removed   区块被回滚，撤销「到账中」
```

一条变动可能直接以 `finalized` 出现；状态只会前进，`id` 始终不变。交易被回滚后重新打包会用新的 `id`，最终只有一个会 `finalized`。

请求体（单区块格式）：

```json
{
  "blockNumber": 124380000,
  "blockHash": "0x9f2c...",
  "timestamp": 1791100000,
  "status": "finalized",
  "events": [
    {
      "id": "evt_8f3c2a91d0b44e6f",
      "status": "finalized",
      "address": "0x你的充值地址",
      "direction": "in",
      "amount": "0.5",
      "amountWei": "500000000000000000",
      "counterparty": "0x转出方",
      "kind": "internal",
      "txHash": "0x...",
      "blockNumber": 124380000,
      "blockHash": "0x9f2c...",
      "timestamp": 1791100000,
      "balance": "3.5",
      "balanceWei": "3500000000000000000"
    }
  ]
}
```

`kind` 是 `external`（交易本身的 value）、`internal`（合约执行中的转账）、`fee`（gas 手续费，固定 `out`）或 `test`。`balance` 是该地址在这个区块结束时的余额。

签名请求头：

```text
X-BSCG-Timestamp: 1791100005
X-BSCG-Signature: sha256=<HMAC-SHA256(密钥, 时间戳 + "." + 原始请求体) 的十六进制>
```

返回任意 2xx 表示整个请求处理成功；其他情况按 1 分钟、5 分钟、30 分钟重试。

## 上生产前要改的地方

这几个示例为了能直接跑，用 JSON 文件存状态。正式使用时：

- **换成数据库**：一个请求在一个事务里处理，提交后再返回 200；变动表以 `id` 为主键，入账表也以 `id` 做唯一约束。
- **入账规则按业务定**：示例把 `in` 记正、`out`（含手续费）记负，得到的就是链上余额变化。充值业务通常只把 `direction = in` 的记到用户余额，其余用于对账。
- **对账用 `balanceWei`**：同一地址两个区块的 `balanceWei` 之差，应等于中间所有 `finalized` 变动之和；对不上就补漏一次。
- **处理要快**：BSC 约 0.75 秒出一个块，接口处理一次超过这个时间会积压。可以先落库再异步处理，或打开「多区块合并推送」。
- **金额用 `amountWei`**：整数字符串，不要用浮点数计算。
- **注意保留期**：变动确认后可查 1 天（每个订阅至少保留最新 5000 条）。宕机超过这个时间，用 `balanceWei` 修正余额。

## 测试

```bash
cd node && npm test
cd python && python3 -m unittest discover -v
cd go && go test ./...
./e2e.sh node   # 或 python / go：启动接收端、跑 send-test.sh、核对账本
```

## 链接

- [BNB Webhook 与 REST 接口文档](https://bscquery.com/docs/bnb?utm_source=github&utm_medium=referral&utm_campaign=examples)
- [BNB 余额监控](https://bscquery.com/bnb?utm_source=github&utm_medium=referral&utm_campaign=examples)
- USDT 等 BEP-20 代币到账、自己合约的事件：见 [bsc-contract-events-graphql](https://github.com/calmw/bsc-contract-events-graphql/blob/main/README.zh-CN.md)。

## 许可证

[MIT](LICENSE)
