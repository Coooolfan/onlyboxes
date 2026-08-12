---
name: release-note-creator
description: Create release notes for Onlyboxes, including determining the change range from historical tags and checking existing release notes against the project writing guidelines.
---

# Release Note Creator

## Instructions
1. 阅读 `references/range-determination.md` 了解如何确定变更范围
2. 从远程获取最新tag列表
3. 与上一步确定的前序tag进行比较，确定变更范围
4. 阅读 `references/release-note-guideline.md` 文件，了解如何撰写更新日志
5. 如版本号、前序tag或更新日志范围存在歧义，再与用户确认
6. 在无歧义时直接撰写更新日志

## tips
- 使用markdown格式
- 不允许使用 `gh` 命令
- 所有`scripts/`下的脚本都支持传递`--help`参数查看使用方法
