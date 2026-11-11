@echo off
setlocal enabledelayedexpansion
cd /d "%~dp0"

set GOTOOLCHAIN=go1.20.14
set CGO_ENABLED=0
set GOOS=windows

if not exist dist mkdir dist

echo === [1/4] agentd  windows/386  (Windows 7 32 位) ===
set GOARCH=386
go build -trimpath -ldflags "-s -w" -o dist\agentd_win7_386.exe .\cmd\agentd
if errorlevel 1 goto fail

echo === [2/4] agentd  windows/amd64 ===
set GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o dist\agentd.exe .\cmd\agentd
if errorlevel 1 goto fail

echo === [3/4] agentctl windows/amd64 ===
set GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o dist\agentctl.exe .\cmd\agentctl
if errorlevel 1 goto fail

echo === [4/4] agentctl windows/386 ===
set GOARCH=386
go build -trimpath -ldflags "-s -w" -o dist\agentctl_386.exe .\cmd\agentctl
if errorlevel 1 goto fail

echo.
echo 构建完成，产物在 dist\ 目录：
dir /b dist
echo.
echo 部署到电脑 C 后，如果自动发现不好用，请在 C 上用管理员 CMD 放行端口：
echo   netsh advfirewall firewall add rule name="juyu agentd tcp" dir=in action=allow protocol=TCP localport=7777
echo   netsh advfirewall firewall add rule name="juyu agentd udp" dir=in action=allow protocol=UDP localport=7778
exit /b 0

:fail
echo.
echo 构建失败，请检查上面的错误输出
exit /b 1
