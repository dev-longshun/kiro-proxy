# Zeabur 部署指南

## 构建镜像

```bash
docker build -t kiro-proxy:latest .
```

推送到你的镜像仓库（以 GitHub Container Registry 为例）：

```bash
docker tag kiro-proxy:latest ghcr.io/<你的用户名>/kiro-proxy:latest
docker push ghcr.io/<你的用户名>/kiro-proxy:latest
```

## 部署步骤

### 1. 创建 MySQL 服务

Add Service → Prebuilt Image → `mysql:8.4`

设置环境变量：

- `MYSQL_ROOT_PASSWORD`：`rabbitcode`
- `MYSQL_DATABASE`：`kiro_proxy`

### 2. 创建 Redis 服务（可选）

Add Service → Prebuilt Image → `redis:7-alpine`

不需要额外配置。如果不部署 Redis，kiro-proxy 会自动降级为内存缓存。

### 3. 创建 kiro-proxy 服务

Add Service → Prebuilt Image → 输入你的镜像地址

### 4. 设置环境变量

在 kiro-proxy 服务中添加环境变量：

| 变量 | 值 | 说明 |
|------|-----|------|
| `API_KEY` | 你的密钥 | API 认证密钥 |

### 5. 设置启动命令

Command：

```
/app/kiro-proxy -port 8989 -mysql "root:rabbitcode@tcp(mysql.zeabur.internal:3306)/kiro_proxy?charset=utf8mb4&parseTime=true&loc=Local" -redis "redis://redis.zeabur.internal:6379"
```

> MySQL/Redis 的内部域名根据你在 Zeabur 中创建的服务名而定，格式为 `<服务名>.zeabur.internal`。

### 6. 开放网络端口

在 Networking 中开放端口 `8989`

## Docker Compose 本地部署（备选）

如果你想在自己的服务器上用 Docker Compose 部署：

```yaml
services:
  mysql:
    image: mysql:8.4
    environment:
      MYSQL_ROOT_PASSWORD: rabbitcode
      MYSQL_DATABASE: kiro_proxy
    volumes:
      - mysql_data:/var/lib/mysql
    restart: unless-stopped

  redis:
    image: redis:7-alpine
    restart: unless-stopped

  kiro-proxy:
    build: .
    ports:
      - "8989:8989"
    environment:
      - API_KEY=你的密钥
    command:
      - -port=8989
      - -mysql=root:rabbitcode@tcp(mysql:3306)/kiro_proxy?charset=utf8mb4&parseTime=true&loc=Local
      - -redis=redis://redis:6379
    depends_on:
      - mysql
      - redis
    restart: unless-stopped

volumes:
  mysql_data:
```

启动：

```bash
docker compose up -d
```

## 不需要配置的项

- **持久化卷** — 数据存在 MySQL 中，kiro-proxy 本身无状态
- **Dockerfile** — 预构建镜像部署不适用

## 常见问题

### 数据库连接失败

确认 MySQL 服务已启动且内部域名正确。Zeabur 中可在 MySQL 服务的 Networking 标签页查看内部地址。

### 启动后无账号

正常现象。访问 `http://<你的域名>/admin` 通过管理后台添加账号。
