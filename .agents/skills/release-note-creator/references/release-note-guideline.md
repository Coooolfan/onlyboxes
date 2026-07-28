# 📘 Release Notes 书写规范

> 本规范用于统一 onlyboxes 仓库的发布说明（Release Notes）书写风格，
> 以保证版本记录简洁、一致、便于追溯，并充分利用 GitHub 的 Markdown 扩展特性。

---

## 🧭 一、总体原则

1. **使用英文撰写**
   - onlyboxes 面向国际用户（README 是英文 + zh-CN 双版本）。
   - **Release notes 必须使用英文**，即使历史 release（如 0.5.0-rc-1、0.6.0）的 body 是中文，也不要照搬。

2. **简洁、扁平、可追溯**
   - 让读者能快速看懂「本次更新做了什么」以及「对应的提交/PR」。
   - 使用简短描述 + 直接链接，避免长段文字。

3. **GitHub 原生兼容**
   - 仅使用 GitHub Flavored Markdown 支持的语法（包括 警报块 Alert Block）。
   - 不额外引入 HTML、表格或图片。

4. **最少维护成本**
   - 一级标题（`# vX.Y.Z`）省略，由 GitHub Release 页面自动显示版本号。
   - tag 不加 `v` 前缀（与历史 release 保持一致，如 `0.6.1-beta-1`、`0.7.0`）。

---

## 🧱 二、结构模板

```markdown
> [!NOTE]
> This release contains important information. Please review whether migration or configuration changes are required.

## feat
* <feature or improvement> <commit-or-PR-URL> <commit-or-PR-URL>

## fix
* <fix description> <commit-or-PR-URL>

## chore / docs
* <maintenance or documentation> <commit-or-PR-URL>

Full Changelog: https://github.com/Coooolfan/onlyboxes/compare/<prev>...<curr>
```

### 说明：

- 各小节使用二级标题（`##`）。
- 条目使用 `*`（星号）列表。
- 所有链接直接贴完整 URL，方便 GitHub 自动识别为超链接。
- 条目末尾**不加句号**。
- 最后一行必须包含 `Full Changelog` 链接，统一格式。

---

## ⚠️ 三、警报块（Alert Block）使用规范

警报块用于强调 **重要、警示或说明性内容**。
它们是 GitHub Markdown 的扩展语法，支持五种类型：

| 类型               | 用途          | 示例语法                                                               | 显示效果         |
| :--------------- | :---------- | :------------------------------------------------------------------ | :----------- |
| `> [!NOTE]`      | 普通提示或版本说明   | `> [!NOTE]\n> This is a beta release.`                              | 蓝色 info      |
| `> [!TIP]`       | 提示技巧或使用建议   | `> [!TIP]\n> Try the new native build option.`                      | 绿色 tip       |
| `> [!IMPORTANT]` | 达成目标所必需的信息  | `> [!IMPORTANT]\n> Update dependencies before building.`            | 紫色 important |
| `> [!WARNING]`   | 紧急信息或潜在问题警告 | `> [!WARNING]\n> Config in this release is not backwards compatible.` | 橙色 warning   |
| `> [!CAUTION]`   | 行动风险或副作用提醒  | `> [!CAUTION]\n> Force-clearing the cache will drop history.`       | 红色 caution   |

> **使用原则：**

```
- 如无必要，无需使用警报块。
- 每个版本说明最多出现一到两个警报块。
- 禁止连续堆叠多个警报（易造成阅读负担）。
- 警报块必须置于文件最上方、正文之前。
- 不得嵌套在列表或代码块中。
```

### ✅ 示例 1：普通提示版本

```markdown
> [!NOTE]
> This release contains a database schema change. The console performs the migration automatically; no manual action is required.
```

### ✅ 示例 2：含 breaking 变更

```markdown
> [!WARNING]
> Breaking change: the legacy `default-work-image-with-browser` Dockerfile is removed. Switch to `coolfan1024/onlyboxes-runtime:default` instead.
> Details: https://github.com/Coooolfan/onlyboxes/commit/0420562
```

---

## 🧩 四、内容分组规则

| 分组                | 内容范围                          | 示例                                                              |
| :---------------- | :---------------------------- | :-------------------------------------------------------------- |
| `## feat`         | 新增功能、改进、性能优化                  | Add LobeHub sandbox runtime image; expose sandbox metadata API   |
| `## fix`          | Bug 修复、稳定性修正                  | Filter terminal resource upload headers to prevent header injection |
| `## chore / docs` | 构建、依赖、文档、脚本                   | Update README; switch release flow to tag-driven pipeline       |
| （可选） `## ci`      | CI/CD 工作流相关变更（独立列出时更易阅读）       | Add deploy-cloudflare job to package-release workflow            |
| （可选） `## perf`    | 性能优化（如需独立说明）                  | Improve metadata snapshot consistency in capability lookup       |

> 不需要的分组可省略，保持简洁。

---

## ✍️ 五、条目书写规范

| 要点   | 说明                          | 示例                                              |
| :--- | :-------------------------- | :---------------------------------------------- |
| 语气   | imperative + 对象 + 结果         | `Add LobeHub sandbox runtime image for browser-heavy workloads` |
| 句号   | 末尾**不加**句号                  | ✅ 正确：句末无句号                                      |
| 链接   | 紧贴描述，以空格分隔                  | `<description> <url>`                           |
| 多链接  | 最少 1 个，空格分隔                  | `<description> <url1> <url2>`                   |
| 重复前缀 | 不写 `feat:` / `chore:` 等 commit 前缀 | ✅ `Update default runtime image` ❌ `feat: update runtime image` |

---

## 📂 六、输出位置

- 发布说明正文写入 `docs/release_notes/<tag>.md`（`<tag>` 与目标 tag 名保持一致，例如 `0.7.1.md`、`0.8.0-beta.1.md`）。
- 文件内容即为符合本规范的 Markdown 正文，不含一级标题（版本号由文件名和 GitHub Release 页面呈现）。

## 🧮 七、辅助说明

若已提供完整 commit 列表，按以下步骤处理：

1. 识别类别关键词（`feat` / `fix` / `chore` / `docs` / `ci` / `breaking` 等）。
2. 自动分组排序（优先级：`breaking` > `feat` > `fix` > `chore/docs/ci`）。
3. 去重、过滤无意义 commit（如 "update version"、"merge branch"、"chore: release X.Y.Z"）。
4. **将中文 commit message 翻译成英文条目**（如果原 commit 是中文）。
5. 输出符合规范的英文 Markdown 文本。

---

## ✅ 八、完整示例

```markdown
> [!NOTE]
> Release flow is now tag-driven. Push a `*.*.*` tag and CI handles draft → upload → publish automatically.

## feat
* Add LobeHub sandbox runtime image and publish three variants (default/default-cn/lobehub) https://github.com/Coooolfan/onlyboxes/pull/7
* Expose sandbox metadata at `GET /api/v1/sandbox/metadata` with limits, capability availability, and worker summary https://github.com/Coooolfan/onlyboxes/commit/8336b2c
* Forward presigned upload headers (x-amz-*, Content-Type, Content-MD5) from MCP export to terminalResource workers https://github.com/Coooolfan/onlyboxes/commit/197c52a

## fix
* Filter terminal resource upload headers through an allowlist to prevent header injection https://github.com/Coooolfan/onlyboxes/commit/f6a49e1
* Share `now`/`offlineTTL` snapshot across capability metadata computation https://github.com/Coooolfan/onlyboxes/commit/116fece

## ci
* Switch package-release workflow to tag-driven (`push: tags '*.*.*'`) with prerelease auto-detection and Cloudflare Workers deploy https://github.com/Coooolfan/onlyboxes/commit/e3b4963

## chore / docs
* Replace hardcoded default tags with GitHub API lookup + fallback in install.py and worker-startup.sh https://github.com/Coooolfan/onlyboxes/commit/e3b4963

Full Changelog: https://github.com/Coooolfan/onlyboxes/compare/0.6.1...0.7.0
```

---

## 🪄 九、总结

| 项 | 规范 |
| :-- | :-- |
| 语言 | **英文** |
| 一级标题 | ❌ 不需要，GitHub 自动生成 |
| 分组标题 | `## feat` / `## fix` / `## ci` / `## chore / docs` |
| 列表符号 | 使用 `*`，每条一句话 |
| 链接格式 | 直接贴完整 URL |
| 警报块 | 用于重要说明，首行前置 |
| Full Changelog | 结尾必须有，格式固定 |
| tag 命名 | 无 `v` 前缀 |
| 最小输入 | commit 列表 + 版本号（上/下） |
| 关键词自动分组 | breaking > feat > fix > chore/ci |
