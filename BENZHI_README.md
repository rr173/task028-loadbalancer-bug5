# task028-loadbalancer Benzhi 评测说明

## 项目用途

`task028-loadbalancer` 是一个纯 Go 内存级负载均衡调度器，提供轮询（RR）、平滑加权轮询（SWRR）和最少连接（LC）三种节点选择策略，并通过 HTTP 接口完成节点注册、健康状态更新、节点选取、连接释放和统计查询。

## 标准 Go 命令

以下命令均在项目根目录执行：

```bash
go build ./...                 # 编译
go run . server --addr :8080   # 启动 HTTP 服务
go run . --smoke-test          # 运行自检
go test ./...                  # 运行测试
go vet ./...                   # 静态检查
```

## Benzhi Docker 构建

使用评测专用的 `benzhi.Dockerfile` 构建镜像，不使用项目自带的 `Dockerfile`：

```bash
chmod +x build_benzhi_docker.sh
./build_benzhi_docker.sh task028-loadbalancer-benzhi linux/amd64
./build_benzhi_docker.sh task028-loadbalancer-benzhi linux/arm64
```

脚本参数依次为镜像名和目标平台，默认值为 `my-project` 与 `linux/amd64`。构建成功后可以进入容器：

```bash
docker run --rm -it task028-loadbalancer-benzhi bash
```
