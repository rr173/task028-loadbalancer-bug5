# task028-loadbalancer

内存级负载均衡调度器，支持轮询、平滑加权轮询（SWRR）、最少连接三种选取策略。

## 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET  | `/healthz` | 健康检查 |
| POST | `/nodes` | 注册/更新节点 `{id, address, weight}` |
| POST | `/nodes/remove` | 移除节点 `{id}` |
| POST | `/health` | 设置健康状态 `{id, healthy}` |
| POST | `/pick` | 选取节点 `{strategy: rr|wrr|lc}`，返回 `{node, address}` |
| POST | `/release` | 释放连接 `{node}` |
| GET  | `/stats` | 全部节点统计，按注册顺序 |

## 策略

- `rr` 轮询：可选节点按注册顺序取模循环。
- `wrr` 平滑加权轮询：nginx 风格 SWRR，高权重节点的选中机会交错分布。
- `lc` 最少连接：选活跃连接数最小者；并列时权重降序、再注册顺序。

「可选节点」= 健康（up）且权重 > 0。选取会占用一条连接（活跃 +1），释放归还一条（活跃 -1）。

## 运行

```bash
go run . server --addr :8080   # 启动服务
go run . --smoke-test          # 自检
```
