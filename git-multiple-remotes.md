# Git 多远程仓库（Multiple Remotes）说明

## 什么是 Remote？

Git 中的 **remote（远程仓库）** 是一个指向远端 Git 仓库的别名。一个本地仓库可以同时配置多个 remote，每个 remote 对应不同的远端地址。

---

## 典型场景：Fork 工作流

当你 Fork 了一个开源项目时，通常会配置两个 remote：

| Remote 名称 | 指向 | 用途 |
|------------|------|------|
| `origin` | 官方原始仓库（如 `golang/go`） | 拉取上游最新代码 |
| `myfork` | 你自己的 Fork（如 `xiaogao688/go`） | 推送你的修改 |

```
官方仓库 (golang/go)          你的 Fork (xiaogao688/go)
      │                               │
      │  git fetch origin             │  git push myfork
      ▼                               ▼
  本地仓库 (你的电脑)  ─────────────────►
```

---

## 常用命令

```bash
# 查看所有 remote
git remote -v

# 添加一个新 remote
git remote add <名称> <地址>

# 删除一个 remote
git remote remove <名称>

# 修改 remote 地址
git remote set-url <名称> <新地址>
```

---

## Fork 工作流的日常操作

### 1. 同步上游更新

```bash
# 拉取官方仓库的最新代码
git fetch origin

# 将上游变更合并到本地分支
git merge origin/master
# 或使用 rebase 保持线性历史
git rebase origin/master
```

### 2. 推送自己的修改

```bash
# 将本地分支推送到自己的 Fork
git push myfork my_go1-25
```

### 3. 发起 Pull Request

将 `myfork` 上的分支推送后，在 GitHub 页面对官方仓库发起 Pull Request，请求上游合并你的修改。

---

## 为什么不直接推送到 origin？

对于大多数开源项目（如 Go 语言官方仓库），普通贡献者**没有直接写入权限**。Fork 工作流的意义在于：

1. 你对自己的 Fork 有完整的写入权限
2. 保持与上游仓库的独立性，不影响官方代码
3. 通过 Pull Request 机制，让官方维护者审查后再合并

---

## remote 名称只是别名

`origin` 和 `myfork` 都只是名字，没有特殊含义。你可以随意命名，但社区有约定俗成的习惯：

- `origin` — 你最主要关联的仓库（clone 时自动创建）
- `upstream` — 上游官方仓库（也有人用这个名字代替 `origin`）
- `myfork` / `fork` — 自己的 Fork
