@echo off
REM 启动 go-llm-proxy（补丁版，支持 DeepSeek thinking 模式工具调用 + 图片翻译）
cd /d %~dp0
go-llm-proxy.exe -config config.yaml
