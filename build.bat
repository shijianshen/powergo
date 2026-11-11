@echo off
setlocal enabledelayedexpansion
cd /d "%~dp0"

rem PowerGo 1.0 构建脚本
rem Go 1.20.14 是最后一个支持 Windows 7 的版本，通过 GOTOOLCHAIN 自动调起
set GOTOOLCHAIN=go1.20.14
set CGO_ENABLED=0
set GOOS=windows

if not exist dist mkdir dist

echo.
echo ============================================
echo   PowerGo 1.0 构建
echo ============================================

echo === [1/5] PGCact  windows/386    (Windows 7 32 位) ===
set GOARCH=386
go build -trimpath -ldflags "-s -w" -o dist\PGCact_386.exe .\cmd\pgcact
if errorlevel 1 goto fail

echo === [2/5] PGCact  windows/amd64 ===
set GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o dist\PGCact_amd64.exe .\cmd\pgcact
if errorlevel 1 goto fail

echo === [3/5] PGAcli  windows/amd64 ===
go build -trimpath -ldflags "-s -w" -o dist\PGAcli.exe .\cmd\pgacli
if errorlevel 1 goto fail

echo === [4/5] PGAcli  windows/386 ===
set GOARCH=386
go build -trimpath -ldflags "-s -w" -o dist\PGAcli_386.exe .\cmd\pgacli
if errorlevel 1 goto fail

echo === [5/5] PGAmcpd windows/amd64 (MCP 适配层) ===
set GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o dist\PGAmcpd.exe .\cmd\pgamcpd
if errorlevel 1 goto fail

echo.
echo 构建完成，产物在 dist\ 目录：
dir /b dist
echo.
echo 部署到被控端后，若自动发现不可用，请用管理员 CMD 放行端口：
echo   netsh advfirewall firewall add rule name="PowerGo TCP" dir=in action=allow protocol=TCP localport=7777
echo   netsh advfirewall firewall add rule name="PowerGo UDP" dir=in action=allow protocol=UDP localport=7778
exit /b 0

:fail
echo.
echo 构建失败，请检查上面的错误输出
exit /b 1
