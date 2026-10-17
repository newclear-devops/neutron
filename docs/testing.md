# Neutron 流水线测试文档

## 测试环境搭建

### 前置条件

- Docker Desktop（含 Kubernetes 或 kind）
- GitLab 实例（Docker 部署）
- MySQL 实例

### 使用 kind 搭建

```bash
# 1. 创建 kind 集群（配置文件在项目根目录）
kind create cluster --name neutron --config kind-config.yaml

# 2. 启动 MySQL（避免与已有实例端口冲突）
docker run -d --name neutron-mysql -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=neutron -p 3307:3306 mysql:8

# 3. 初始化数据库
docker exec -i neutron-mysql mysql -uroot -proot neutron < dds.sql

# 4. 将 GitLab 和 MySQL 连接到 kind 网络
docker network connect kind gitlab-ce
docker network connect kind neutron-mysql

# 5. 创建 K8s Secret
kubectl create secret generic git-ssh-secret --from-file=id_rsa=$HOME/.ssh/id_ed25519

# 6. 构建并加载镜像（API server + runner）
make kind-load

# 7. 部署
kubectl apply -f k8s-deploy.yaml
```

### 配置 GitLab

1. 添加 SSH 公钥到 GitLab（User Settings → SSH Keys）
2. 允许本地网络请求（Admin → Settings → Network → Outbound requests → 勾选 Allow requests to the local network from hooks and services）

---

## 镜像要求

每个 K8s Job 创建三个容器，对镜像有不同要求：

| 容器 | 用途 | 必需工具 | 配置来源 |
|------|------|---------|---------|
| checkout (init) | clone 仓库、MR 合并 | `git`, `ssh` 客户端 | 复用 job 的 `image` |
| init (init) | 从 runner 镜像复制二进制 | `cp`（busybox 自带） | `config.yaml` → `init-image`（runner 镜像） |
| pipeline (main) | 执行流水线步骤 | `/bin/sh` + 业务依赖 | `neutron.yaml` → `image` |

**注意：** checkout 容器复用 pipeline 的 image，因此 `image` 必须包含 `git` 和 SSH 客户端。`alpine:latest` 等精简镜像不包含这些工具，会导致 git clone 失败。

推荐镜像：
- `alpine/git:latest` — 通用，含 git 和 ssh
- `node:18-alpine` — Node.js 项目
- `golang:1.23-alpine` — Go 项目
- `neutron-runner:local` — init 容器专用（由 `make docker-runner` 构建）

---

## 触发场景测试

### 场景 1：PUSH 触发

**前置条件：** `neutron.yaml` 中 job 配置 `trigger: ["PUSH"]`

**操作：** push 代码到任意分支

**预期行为：**
- 仅执行 `trigger` 包含 `PUSH` 的 job
- 每个步骤的状态依次上报到 GitLab（pending → running → success/fail）
- K8s Job 最终状态为 Complete 或 Failed

**验证命令：**
```bash
kubectl get jobs -o custom-columns='NAME:.metadata.name,STATUS:.status.succeeded,TRIGGER:.metadata.annotations.triggerType'
kubectl logs job/<job-name> -c pipeline
```

**测试结果：** ✅ 通过

---

### 场景 2：MR 无冲突

**前置条件：**
- `neutron.yaml` 中 job 配置 `trigger: ["MR"]`
- 源分支与目标分支无文件冲突

**操作：**
1. 创建源分支，修改文件（不影响目标分支的文件）
2. Push 源分支
3. 创建 MR

**预期行为：**
- 自动合并源分支到目标分支后执行流水线
- 合并后的代码同时包含源分支和目标分支的变更
- 仅执行 `trigger` 包含 `MR` 的 job

**验证命令：**
```bash
# 检查 Job 的 checkout 日志，确认合并成功
kubectl logs job/<job-name> -c checkout
# 检查 pipeline 日志，确认合并后的文件存在
kubectl logs job/<job-name> -c pipeline
```

**测试结果：** ✅ 通过

---

### 场景 3：MR 有冲突

**前置条件：**
- `neutron.yaml` 中 job 配置 `trigger: ["MR"]`
- 源分支与目标分支修改了同一文件的同一位置

**操作：**
1. 在源分支修改 `conflict.txt`
2. 在目标分支修改 `conflict.txt`（不同内容）
3. 创建 MR

**预期行为：**
- `git merge` 失败，init container 报错
- K8s Job 立即标记为 Failed（`backoffLimit: 0`，不重试）
- 流水线不会执行（主容器不会启动）

**验证命令：**
```bash
# 确认 Job 失败
kubectl get jobs
# 查看 checkout 容器日志，确认冲突信息
kubectl logs job/<job-name> -c checkout
```

**测试结果：** ✅ 通过（需 `backoffLimit: 0` 修复）

**已知限制：** 冲突时不会向 GitLab 上报失败状态，MR 页面无流水线信息。

---

### 场景 4：TAG 触发

**前置条件：**
- `neutron.yaml` 中 job 配置 `trigger: ["TAG"]`

**操作：**
```bash
git tag v1.0.0
git push origin v1.0.0
```

**预期行为：**
- 仅执行 `trigger` 包含 `TAG` 的 job
- checkout 特定的 tag commit

**测试结果：** ✅ 通过

---

### 场景 5：触发类型过滤

**验证：** 不同触发类型只执行对应的 job

| 触发类型 | `build` job (trigger: PUSH, MR) | `release` job (trigger: TAG) |
|---------|--------------------------------|------------------------------|
| PUSH    | ✅ 执行 | ⏭ 跳过 |
| MR      | ✅ 执行 | ⏭ 跳过 |
| TAG     | ⏭ 跳过 | ✅ 执行 |

**测试结果：** ✅ 通过

---

### 场景 6：Shell 特性支持

**前置条件：** `neutron.yaml` 中 step 使用 shell 特性（管道、重定向等）

```yaml
steps:
  - name: pipe-test
    cmd: echo "hello world" | grep hello
  - name: redirect-test
    cmd: echo "test" > /tmp/test.txt && cat /tmp/test.txt
```

**预期行为：** 命令通过 `sh -c` 执行，支持管道、重定向、`&&` 等 shell 特性

**测试结果：** ✅ 通过（`sh -c` 修复后）

---

## 待测试场景

### 场景 7：MR 修改 neutron.yaml

**目的：** 验证 MR 中修改了流水线定义时，使用源分支的 `neutron.yaml`

**操作：**
1. 在源分支修改 `neutron.yaml`（增加/修改 step）
2. 创建 MR

**预期行为：** 使用源分支的 `neutron.yaml` 执行流水线（因为 `Parse()` 用源 commit SHA 获取文件）

---

### 场景 8：多 Job 并行

**目的：** 验证 `neutron.yaml` 中定义多个同触发类型的 job 时，并行创建

```yaml
jobs:
  lint:
    image: alpine:latest
    trigger: ["PUSH"]
    steps:
      - name: lint
        cmd: echo "linting..."
  test:
    image: alpine:latest
    trigger: ["PUSH"]
    steps:
      - name: test
        cmd: echo "testing..."
```

**预期行为：** 同时创建两个 K8s Job，各自独立执行

---

### 场景 9：空步骤

**目的：** 验证 job 没有 steps 时的行为

```yaml
jobs:
  empty:
    image: alpine:latest
    trigger: ["PUSH"]
    steps: []
```

**预期行为：** Job 完成但不产生任何状态上报

---

### 场景 10：无效镜像

**目的：** 验证 job 指定不存在的 Docker 镜像时的行为

```yaml
jobs:
  bad-image:
    image: nonexistent/image:latest
    trigger: ["PUSH"]
    steps:
      - name: hello
        cmd: echo "hello"
```

**预期行为：** K8s Pod 因镜像拉取失败而 Error，Job 最终 Failed

---

### 场景 11：步骤失败后的行为

**目的：** 验证中间步骤失败时，后续步骤的状态

```yaml
jobs:
  fail-test:
    image: alpine:latest
    trigger: ["PUSH"]
    steps:
      - name: step1
        cmd: echo "step1"
      - name: step2
        cmd: exit 1
      - name: step3
        cmd: echo "step3"
```

**预期行为：**
- step1: Success
- step2: Fail（附带具体错误信息）
- step3: Fail（标记为 pipeline failed）
- Job 状态: Failed

---

### 场景 12：多个 MR 与 Job 并发策略

**目的：** 验证并发限制按项目和逻辑 job key 生效，而不是按 MR 数量生效

**操作：** 同一项目中保持一个 MR 的 job 运行，再创建另一个会触发相同 job key 的 MR；随后用 `parallel: true` 重复验证。

**预期行为：** 默认配置下，第二次触发记为 `skipped`，不会创建第二个 K8s Job；设置 `parallel: true` 后允许两个 K8s Job 同时运行。不同 job key 仍可并行。

---

### 场景 13：Force Push

**目的：** 验证 force push 后的行为

**操作：**
```bash
git commit --amend -m "amended"
git push --force origin main
```

**预期行为：** 触发新的 PUSH 流水线，使用新的 commit SHA

---

### 场景 14：Webhook 重放

**目的：** 验证 GitLab 重放 webhook 时的行为

**操作：** 在 GitLab Webhook 设置中点击 "Resend" 重放事件

**预期行为：** 如果原 job 仍未结束，重放会记录为 `skipped`，Recent Jobs 中能看到跳过记录，且不会创建第二个 K8s Job；如果原 job 已结束，则按正常规则触发新流水线。

---

### 场景 15：neutron.yaml 不存在

**目的：** 验证仓库中没有 `neutron.yaml` 时的行为

**预期行为：**
- API Server 返回错误（GitLab API 返回 404）
- 不创建 K8s Job
- 返回 HTTP 400 给 GitLab

---

### 场景 16：重复触发、rerun 与失败关闭

**目的：** 验证 webhook、手动 API trigger 和 rerun 共用同一并发规则。

**操作：**
1. 让一个 job 持续运行，再通过 webhook 重放或 `/api/trigger` 触发同一项目、同一 job key。
2. 对该 job 使用 Rerun；确认该次请求也被跳过。
3. 检查 Recent Jobs 和项目历史中的 `skipped` 记录及 API 响应。
4. 在隔离测试环境中让 API 无法读取已有 K8s Job，再尝试触发同一 job。

**预期行为：** 前三步都记录 `skipped` 且不创建新 K8s Job；最后一步触发失败且不创建新 job。恢复 Kubernetes 访问后可正常重试。需要重跑时先手动 Kill 现有 job。

---

## 测试命令速查

```bash
# 查看所有 Job
kubectl get jobs -o custom-columns='NAME:.metadata.name,STATUS:.status.succeeded,TRIGGER:.metadata.annotations.triggerType'

# 查看所有 Pod
kubectl get pods

# 查看流水线日志
kubectl logs job/<job-name> -c pipeline

# 查看 init container 日志（git clone）
kubectl logs job/<job-name> -c checkout

# 查看 API Server 日志
kubectl logs -l app=neutron-api

# 查看 GitLab webhook 投递记录
# GitLab → Project → Settings → Webhooks → 点击 hook 查看 Recent deliveries

# 查看 Job 状态（JSON API）
curl http://localhost:8888/api/status/<job-name>
```
