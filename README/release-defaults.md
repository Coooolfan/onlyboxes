# 发版默认值检查

本文档记录发版前需要同步检查的默认版本与默认文档项。

## Release tag 默认值

发版前应确认以下位置的默认版本均已更新到当前目标 release tag：

- `scripts/install.py`
  - `DEFAULT_TAG`
- `README.md`
  - 一键安装参数表中的 `--tag` 默认值
- `README.zh-CN.md`
  - 一键安装参数表中的 `--tag` 默认值
- `website/src/docs/en/install.mdx`
  - 安装文档参数表中的 `--tag` 默认值
- `website/src/docs/zh-CN/install.mdx`
  - 安装文档参数表中的 `--tag` 默认值
- `website/src/features/docs/TagSelector.tsx`
  - `defaultTag`
- `web/public/static/worker-startup.sh`
  - `DEFAULT_TAG`
  - Usage 文本中 `--tag` 的 `Defaults to ...` 描述
- `web/vite.config.ts`
  - `workerStartupDefaultTag` 的回退字符串

## Worker Sys Temporary Probe 默认值

发版前应确认 web 工程中的 Temporary Probe 默认安装版本与当前目标 release tag 一致：

- `web/src/composables/useWorkerStartupTool.ts`
  - `defaultTemporaryProbeInstallerTag`
- `web/src/components/worker-tool/WorkerSysConfigForm.vue`
  - Temporary Probe release tag 表单说明
  - Temporary Probe release tag 输入框 `placeholder`

## Runtime 镜像默认值

发版前应确认 terminal runtime 镜像的仓库名、稳定别名和版本化 tag 约定保持一致：

- `.github/workflows/package-release.yml`
  - `push-runtime-images` job 的 `coolfan1024/onlyboxes-runtime` 仓库名
  - 版本化 tag：`<release>-default`、`<release>-default-cn`、`<release>-lobehub`
  - 稳定别名：`default`、`default-cn`、`lobehub`、`latest`（指向 `default`）
- `README.md` / `README.zh-CN.md`
  - `WORKER_TERMINAL_EXEC_DOCKER_IMAGE`
  - 发布与镜像章节中的 runtime 镜像说明
- `website/src/docs/en/worker-docker.mdx` / `website/src/docs/zh-CN/worker-docker.mdx`
  - `WORKER_TERMINAL_EXEC_DOCKER_IMAGE`
- `website/src/docs/en/worker-boxlite.mdx` / `website/src/docs/zh-CN/worker-boxlite.mdx`
  - `WORKER_TERMINAL_EXEC_BOXLITE_IMAGE`
- `web/src/composables/useWorkerStartupTool.ts`
  - `defaultTerminalExecDockerImage`
  - `defaultTerminalExecBoxliteImage`

## 复查建议

发版前至少检查旧版本号是否仍出现在上述文件中，避免安装脚本、根文档、官网文档和控制台生成命令的默认版本不一致。
