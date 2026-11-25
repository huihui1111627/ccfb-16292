# 核对记录

## 全量固定种子核对

命令：

```bash
go run ./cmd/verify -scale full
```

规模：

- seed：`20261126`
- 初始订阅：`64 * 3750 = 240000`
- 动态新增订阅：`6400`
- 生成订阅总数：`246400`
- 删除初始订阅：`6400`
- 消息：`2400000`
- 门店：`64`

结果（2026-11-26 本机，最终修复后复跑）：

```text
核对结果: OK=true
消息数=2400000 门店数=64 活跃条件判断次数=321206608604 用时=1139117ms
全量索引匹配基准: 246400 订阅, avg=3807.380µs/msg, p95=6969.333µs, max=9797.833µs
```

核对方式：重新生成同种子消息，按 `AddSeq <= seq < DeleteSeq` 构造该消息时刻的活跃订阅；
对每家门店的每条活跃订阅直接执行合取条件判断，得到期望投递门店，再与仿真逐消息保存的
`uint64` 门店位掩码比较。`OK=true` 表示 240 万条消息 × 64 家门店没有漏投或多投。

小规模快速核对与随机索引等价性、合并覆盖性、边界、多值、缺失策略测试：

```bash
go test ./...
go test -race ./internal/pawn ./internal/web
go run ./cmd/verify -scale small
```

最终代码上 `go test ./...` 与关键包 `-race` 均通过；Web 包有一个在仿真回放期间并发轮询 `/api/state` 的 race 回归测试。

各级订阅表大小和链路流量统计：

```bash
go run ./cmd/stats -scale full
```

一次完整统计的摘要（同机，约 14 分 30 秒回放）：

```text
nodes=72 current_links=71
HQ:   received=11886 reported=7430 avg_match=260.09us
R1:   received=1314  reported=309
R2:   received=1673  reported=664
R3:   received=1501  reported=113
R5:   received=4317  reported=1495
R6:   received=1285  reported=302
R7:   received=1955  reported=793
R8:   received=4951  reported=1671
S01:  raw=3733 received=3733 reported=186
S02:  raw=3732 received=3732 reported=243
...
S64:  raw=3745 received=3745 reported=607

所有当前/已结束链路合计:
up=4,668,320 down=83,764,767 necessary=83,764,767 extra=0
```

`extra=0` 是链路/消息粒度：只要一条下行消息最终在该子树有任一精确接收者，这条链路转发就仍有必要。
合并过滤器相对某条来源订阅可能更宽，但同店/同子树的其它订阅常常也会命中；小规模统计中可以看到非零 `extra`。
页面同时展示每条链路的 `down - necessary` 和样本合并估计超投率。
