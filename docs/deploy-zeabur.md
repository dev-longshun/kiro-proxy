# Zeabur 部署指南

## 镜像地址

```
ghcr.io/dev-longshun/kiro-proxy:latest
```

## 部署步骤

### 1. 创建 MySQL 服务

Add Service → Marketplace → MySQL

Zeabur 会自动生成连接变量，记下 `MYSQL_HOST`、`MYSQL_PORT`、`MYSQL_PASSWORD`。

### 2. 创建 Redis 服务（可选）

Add Service → Marketplace → Redis

不部署 Redis 也行，kiro-proxy 会自动降级为内存缓存。

### 3. 创建 kiro-proxy 服务

Add Service → Prebuilt Image → 输入上方镜像地址

### 4. 设置环境变量

在 kiro-proxy 服务的 Variables 中添加：

| 变量 | 值 | 说明 |
|------|-----|------|
| `API_KEY` | 你的密钥 | API 认证密钥 |

### 5. 设置启动命令

Command：

```
/app/kiro-proxy -port 8989 -mysql "${MYSQL_USERNAME}:${MYSQL_PASSWORD}@tcp(${MYSQL_HOST}:${MYSQL_PORT})/kiro_proxy?charset=utf8mb4&parseTime=true&loc=Local" -redis "redis://${REDIS_HOST}:${REDIS_PORT}"
```

> 使用 Zeabur 的变量引用语法，会自动注入同项目内 MySQL/Redis 服务的连接信息。

### 6. 开放网络端口

在 Networking 中开放端口 `8989`

## 不需要配置的项

- **持久化卷** — 数据存在 MySQL 中，kiro-proxy 本身无状态
- **Dockerfile** — 预构建镜像部署不适用

## 版本标签

- `latest` — 打 `v*` tag 时更新（正式版本）
- `beta` — 每次推送到 `main` 分支时更新

## 常见问题

### 数据库连接失败

确认 MySQL 服务已创建且在同一个 Project 内。Zeabur 的变量引用需要服务间在同一项目才能互相发现。

### 启动后无账号

正常现象。访问 `https://<你的域名>/admin` 通过管理后台添加账号。
