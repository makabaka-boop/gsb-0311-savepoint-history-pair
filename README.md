# txcheck — 数据库操作日志审计器

一段日志可能**最终写出的数值完全正确**，却仍然包含读未提交、先于来源事务提交、冲突环等风险——只看最终状态无法解释。`txcheck` 是一个 Go 命令行审计器：读取一份 JSON 操作日志，按**日志顺序**（而非最终状态）独立判定各项性质，并给出首个违反操作。

## 输入格式

```json
{
  "transactions": ["T1", "T2"],
  "ops": [
    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
    {"txn": "T2", "op": "READ",  "key": "x"},
    {"txn": "T2", "op": "COMMIT"},
    {"txn": "T1", "op": "ABORT"}
  ]
}
```

校验规则（违反即报错，退出码 1，错误以 JSON 输出到 stderr）：

- 2～8 个不同的事务 id（非空字符串）；
- 至多 500 个按序操作，`op ∈ {READ, WRITE, SAVEPOINT, ROLLBACK, COMMIT, ABORT}`；
- READ/WRITE 必须带 `key`；WRITE 可带整数 `value`（缺省 0）；SAVEPOINT/ROLLBACK 使用 1～32 位 ASCII 字母、数字、下划线或连字符组成的 `name`，不得带 `key`；COMMIT/ABORT 不得带 `key` 或 `name`；
- 保存点属于单个事务；同一事务内生效的保存点不得重名。ROLLBACK 只能指向本事务仍生效的保存点，较晚的保存点随回退失效，被指向的保存点仍可再次回退；
- 每个事务**恰有一个**终止操作（COMMIT 或 ABORT），且终止后不得再出现该事务的操作。

## 判定规则（精确定义）

**读来源**：每个 READ 的来源是同一键上此前最近一次仍生效的 WRITE；没有则为初始版本（`"kind": "initial"`）。ABORT 不会改变已经发生的读或写；ROLLBACK 使本事务在目标保存点之后的读写不再参与后续读值选择与提交依赖，但已发生的读取仍留在审计记录中。

**冲突图**：不同事务对同一键已经执行过的冲突操作对（RW、WR、WW；RR 不算）按日志顺序构成有向边 `前操作事务 → 后操作事务`；后来回退不会抹掉物理操作曾发生的事实。

- 图无环 → 输出 **id 字节序最小**的串行顺序（Kahn 算法每次取字节序最小的可发射节点，即字典序最小的拓扑序；字节序下 `"T10" < "T2"`）；
- 有环 → 输出一个**真实有向环**（首尾相接、每条相邻边都真实存在于冲突图）。

**三项性质**（各自独立判定，分别报告首个违反操作；已发生的脏读仍计入无级联读和严格性，读者回退该读取后则不再成为其提交依赖；若读者保留了对后来被回退写入的读取，其 COMMIT 仍违反可恢复性）：

| 性质 | 定义 | 首个违反操作 |
|---|---|---|
| `recoverable` 可恢复 | 保留下来的读取不得依赖未提交事务或后来被回退的写入 | 某 COMMIT：其仍生效的读取所依赖的来源事务此刻未提交，或来源写入已被回退（即使写者后来提交） |
| `cascadeless` 无级联读 | 只读已提交的写入 | 某 READ：来源写入（他事务）此刻未提交 |
| `strict` 严格 | 不读、不写他人未提交的写入 | 某 READ/WRITE：该键最近一次他事务写入此刻未提交 |

## 输出

成功时向 stdout 输出 JSON 报告（退出码 0）：

- `reads`：每个 READ 的来源（写入序号/事务/值，或初始版本）；
- `rollbacks`：每次回退输出 `{seq, txn, name, undoneWrites, discardedReads, affectedReads}`。三个序号数组均升序且不重复；前两个只列本次新失效的该事务写入、读取，`affectedReads` 列所有在本次回退前读过这些写入的历史 READ，包括后来被读者回退的记录；
- `edges`：冲突图的边及全部冲突操作对（键、类型 RW/WR/WW、双方序号）；
- `serializability`：`acyclic` + `order`（无环）或 `cycle`（有环）；
- `recoverable` / `cascadeless` / `strict`：`ok` 及首个违反操作（序号、事务、操作、键、原因）；
- `finalState`：仅已提交且未被保存点回退的写入按日志顺序应用的最终状态——用于对照：它可能看起来完全正常，而上面的性质已被违反。

## 运行

本地（需 Go ≥ 1.23）：

```sh
go build -o txcheck .
./txcheck examples/dirty-read.json     # 读文件
./txcheck < examples/cycle.json        # 读 stdin 管道
./txcheck examples/savepoint.json      # 保存点回退：读后重读、affectedReads 与可恢复性
```

Compose 的 `txcheck` 服务（镜像构建时会先跑 `go vet` 和全部测试）：

```sh
docker compose build txcheck
docker compose run --rm txcheck /data/dirty-read.json    # examples/ 挂载为 /data
docker compose run -T --rm txcheck < examples/cycle.json # 或经 stdin 管道
```

## 测试

```sh
go test ./...
```

- `audit_test.go`：手工小日志用例（脏读后双双撤销、不可恢复、可恢复但非无级联、盲写覆盖、冲突环、撤销写入仍是读来源、初始版本与自读、字节序拓扑、输入校验等），断言读来源、图边、串行序/环与三项性质的首个违反序号；
- `reference_test.go`：一个独立的**参考解释器**（暴力实现：逐读回扫求来源、全对枚举求边、全排列枚举求最小串行序、按定义直算三项性质），对 3000 条随机生成的小日志逐条交叉核对审计器的读来源、图边、无环时的串行序、有环时环的真实性（闭合、简单、每条边都在冲突图中）以及三项性质的首个违反操作。

## 项目结构

```
main.go            CLI 入口（stdin/文件 → JSON 报告）
audit.go           解析校验 + 审计核心（读来源、冲突图、拓扑/找环、三项性质）
audit_test.go      手工小日志用例
reference_test.go  参考解释器 + 随机交叉核对
examples/          示例日志（dirty-read / cycle / clean）
Dockerfile         多阶段构建（构建期运行 vet 与测试）
compose.yaml       txcheck 服务
```
