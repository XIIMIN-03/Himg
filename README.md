# Himg

Himg 是一个基于 Go 的轻量级图片托管系统，除基础功能外支持 AI 标签、AI 审核以及 本地/WebDAV/S3 存储。

## 功能特性

- 图片上传后自动转换为 WebP，并返回可访问链接。
- 支持游客上传、后台图片管理、重命名、隐藏、删除和访问统计。
- 支持公告配置、主题管理、站点标题与首页文案配置。
- 支持本地目录、WebDAV、S3 兼容对象存储。
- 支持 AI 标签生成、AI 内容审核和关键词搜索。
- 支持 Server 酱、邮件 SMTP、Telegram 通知。
- 内置基础安全配置，包括访问限速、请求体限制、IP 黑名单和安全响应头。

## 技术栈

- Go
- SQLite

## 目录结构

```text
.
├── code/                 # Go 后端源码
├── data/                 # 运行数据目录，不建议提交到公开仓库
├── docker/               # Docker 构建与编排配置
└── themes/default/       # 默认前端主题
```

## 预览图
![](https://himg.xiimin.com/uploads/1777995781149292975.webp)

## 快速开始

### 1. 使用 Docker Compose 启动

```bash
cd docker
docker compose up -d --build
```

启动后访问：

```text
http://localhost:6841
```

后台登录使用 `.env` 中的 `HIMG_PASSWORD`。

## 本地开发

进入源码目录运行：

```bash
cd code
go mod download
go run .
```

默认服务监听：

```text
http://127.0.0.1:8080
```

## 常用环境变量

| 变量 | 说明 | 默认值 |
| --- | --- | --- |
| `HIMG_PASSWORD` | 管理员登录密码 | `admin123` |
| `HIMG_TOKEN` | 登录 Cookie Token，不配置时自动生成 | 自动生成 |
| `HIMG_DATA_DIR` | 数据目录 | `data` |
| `HIMG_DB_FILE` | SQLite 数据库文件 | `data/himg.db` |
| `HIMG_THEMES_DIR` | 主题目录 | `themes` |
| `LOCAL_UPLOAD_DIR` | 本地上传目录 | `data/uploads` |
| `HIMG_STORAGE` | 存储类型：`local`、`webdav`、`s3` | `local` |
| `HIMG_PUBLIC_BASE_URL` | 图片公开访问基础地址 | 空 |

### WebDAV 存储

```env
HIMG_STORAGE=webdav
WEBDAV_URL=https://dav.example.com
WEBDAV_USER=your-user
WEBDAV_PASSWORD=your-password
WEBDAV_BASE_PATH=himg/uploads
HIMG_PUBLIC_BASE_URL=https://cdn.example.com
```

### S3 兼容存储

```env
HIMG_STORAGE=s3
S3_ENDPOINT=s3.example.com
S3_ACCESS_KEY=your-access-key
S3_SECRET_KEY=your-secret-key
S3_BUCKET=your-bucket
S3_REGION=auto
S3_USE_SSL=true
S3_PREFIX=himg/uploads
HIMG_PUBLIC_BASE_URL=https://cdn.example.com
```

## API 示例

上传图片：

```bash
curl -X POST "http://localhost:6841/api/upload" \
  -F "image=@/path/to/image.png"
```

下载图片：

```bash
curl -O "http://localhost:6841/uploads/example.webp"
```

需要管理员权限的 API 支持以下认证方式：

```bash
curl -H "X-API-Password: your-password" "http://localhost:6841/api/images"
```

或：

```bash
curl -H "Authorization: Bearer your-password" "http://localhost:6841/api/images"
```
